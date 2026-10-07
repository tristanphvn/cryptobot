// Package handlers — websocket Hub.
//
// Architecture (production grade):
//
//   - Hub.run() owns the clients map. Register/unregister/broadcast all
//     happen on channels read by run(); no mutex on the hot path.
//   - Each client owns two goroutines: readPump (handles pongs + close
//     detection) and writePump (serialises all writes, enforces write
//     deadline, sends app-level pings).
//   - Each client has a bounded send channel. If a client cannot keep up,
//     the hub drops the client (closes its send channel) instead of
//     blocking the broadcast goroutine. One slow tab can NEVER stall
//     the realtime pipeline.
//   - The hub subscribes to the event bus once and translates events
//     into broadcast messages. Wire this with hub.Run(ctx, bus).
package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"context"
	"cryptobot/internal/eventbus"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

const (
	wsClientWriteWait    = 10 * time.Second
	wsClientPongWait     = 60 * time.Second
	wsClientPingInterval = 30 * time.Second
	wsClientMaxMsgSize   = 1 << 20 // 1 MiB inbound (we expect tiny client messages)
	wsClientSendBuf      = 256     // outbound queue per client
)

// newUpgrader only upgrades browser requests whose Origin is in
// allowed (CRY-75). Without the check any web page the operator visits
// could open a socket to the local backend and read the live feed.
// Requests without an Origin header (non-browser clients, tests) pass.
func newUpgrader(allowed []string) websocket.Upgrader {
	set := make(map[string]bool, len(allowed))
	for _, o := range allowed {
		set[normalizeOrigin(o)] = true
	}
	return websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin: func(r *http.Request) bool {
			origin := r.Header.Get("Origin")
			return origin == "" || set[normalizeOrigin(origin)]
		},
	}
}

func normalizeOrigin(o string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(o)), "/")
}

// outbound message: any JSON-serialisable value.
type wsMsg = map[string]any

type client struct {
	conn   *websocket.Conn
	send   chan wsMsg
	remote string
}

// Hub fan-outs broadcast messages to all connected clients.
type Hub struct {
	register   chan *client
	unregister chan *client
	broadcast  chan wsMsg

	clientsCount atomic.Int32
	totalDropped atomic.Uint64
	totalSent    atomic.Uint64

	upgrader websocket.Upgrader
}

// NewHub builds a hub that accepts browser connections only from
// allowedOrigins (see newUpgrader).
func NewHub(allowedOrigins []string) *Hub {
	return &Hub{
		register:   make(chan *client, 16),
		unregister: make(chan *client, 16),
		broadcast:  make(chan wsMsg, 1024),
		upgrader:   newUpgrader(allowedOrigins),
	}
}

// Health returns counters for diagnostics.
func (h *Hub) Health() map[string]any {
	return map[string]any{
		"clients":         h.clientsCount.Load(),
		"messages_sent":   h.totalSent.Load(),
		"messages_queued": len(h.broadcast),
		"clients_dropped": h.totalDropped.Load(),
	}
}

// Run starts the hub goroutine and subscribes to the event bus.
// Blocks until ctx is cancelled. Launch in its own goroutine.
func (h *Hub) Run(ctx context.Context, bus *eventbus.Bus) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[hub] panic recovered: %v", r)
		}
	}()

	tickSub := bus.Subscribe(eventbus.TopicCandleTick, 512, "hub.tick")
	defer tickSub.Close()
	closedSub := bus.Subscribe(eventbus.TopicCandleClosed, 256, "hub.closed")
	defer closedSub.Close()

	clients := make(map[*client]struct{})

	// Helper: drop a client cleanly.
	drop := func(c *client) {
		if _, ok := clients[c]; !ok {
			return
		}
		delete(clients, c)
		close(c.send)
		h.clientsCount.Store(int32(len(clients)))
		log.Printf("[hub] client removed: %s (total=%d)", c.remote, len(clients))
	}

	log.Println("[hub] started")
	for {
		select {
		case <-ctx.Done():
			log.Println("[hub] shutting down — closing all clients")
			for c := range clients {
				close(c.send)
			}
			return

		case c := <-h.register:
			clients[c] = struct{}{}
			h.clientsCount.Store(int32(len(clients)))
			log.Printf("[hub] client added: %s (total=%d)", c.remote, len(clients))

		case c := <-h.unregister:
			drop(c)

		case msg := <-h.broadcast:
			h.fanout(clients, msg, drop)

		case raw, ok := <-tickSub.C:
			if !ok {
				return
			}
			if evt, ok := raw.(eventbus.CandleEvent); ok {
				h.fanout(clients, wsMsg{"type": "candle", "data": evt.Candle, "closed": evt.IsClosed}, drop)
			}

		case raw, ok := <-closedSub.C:
			if !ok {
				return
			}
			if evt, ok := raw.(eventbus.CandleEvent); ok {
				h.fanout(clients, wsMsg{"type": "candle.closed", "data": evt.Candle}, drop)
			}
		}
	}
}

// fanout pushes msg into every client's send buffer non-blockingly.
// Slow clients are dropped instead of stalling the hub.
func (h *Hub) fanout(clients map[*client]struct{}, msg wsMsg, drop func(*client)) {
	for c := range clients {
		select {
		case c.send <- msg:
			h.totalSent.Add(1)
		default:
			// Client cannot keep up — drop it. The frontend will reconnect.
			h.totalDropped.Add(1)
			log.Printf("[hub] DROP slow client %s (send buffer full)", c.remote)
			drop(c)
		}
	}
}

// Broadcast is kept for callers outside the bus (e.g. trade events).
// Non-blocking: if the hub's broadcast channel is saturated, the
// message is dropped and a counter is bumped.
func (h *Hub) Broadcast(msg map[string]any) {
	select {
	case h.broadcast <- msg:
	default:
		h.totalDropped.Add(1)
		log.Println("[hub] DROP broadcast — hub queue full")
	}
}

// HandleWS upgrades the HTTP request to a websocket and registers the client.
func (h *Hub) HandleWS(c *gin.Context) {
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("[ws] upgrade error: %v", err)
		return
	}

	cl := &client{
		conn:   conn,
		send:   make(chan wsMsg, wsClientSendBuf),
		remote: c.Request.RemoteAddr,
	}
	log.Printf("[ws] client connecting: %s", cl.remote)

	// Configure read side BEFORE registering so the first pong handler
	// is in place when the read pump starts.
	conn.SetReadLimit(wsClientMaxMsgSize)
	_ = conn.SetReadDeadline(time.Now().Add(wsClientPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsClientPongWait))
	})

	h.register <- cl

	go h.writePump(cl)
	go h.readPump(cl)
}

// writePump owns conn writes. Sends queued messages, periodic pings,
// and enforces a write deadline so a stuck client is detected fast.
func (h *Hub) writePump(c *client) {
	ticker := time.NewTicker(wsClientPingInterval)
	defer func() {
		ticker.Stop()
		_ = c.conn.Close()
		// Best-effort unregister; safe to send on a buffered channel.
		select {
		case h.unregister <- c:
		default:
		}
	}()

	for {
		select {
		case msg, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(wsClientWriteWait))
			if !ok {
				// Hub closed the channel — politely tell the client.
				_ = c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := c.conn.WriteJSON(msg); err != nil {
				log.Printf("[ws] write error %s: %v", c.remote, err)
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(wsClientWriteWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Printf("[ws] ping error %s: %v", c.remote, err)
				return
			}
		}
	}
}

// readPump exists primarily to detect close + drive pong handler.
// It also echoes application-level `{type:"ping",ts:N}` frames back as
// `{type:"pong",ts:N}` so the browser can measure true round-trip time
// (the WS protocol-level ping/pong is opaque to browser JS).
func (h *Hub) readPump(c *client) {
	defer func() {
		select {
		case h.unregister <- c:
		default:
		}
		_ = c.conn.Close()
	}()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[ws] read error %s: %v", c.remote, err)
			}
			return
		}
		// Best-effort decode; non-JSON or unknown types are ignored.
		var probe struct {
			Type string `json:"type"`
			Ts   int64  `json:"ts"`
		}
		if err := json.Unmarshal(data, &probe); err != nil {
			continue
		}
		if probe.Type == "ping" {
			select {
			case c.send <- wsMsg{"type": "pong", "ts": probe.Ts}:
			default:
				// writePump is backed up — the client is slow; fanout
				// will drop on the next broadcast.
			}
		}
	}
}
