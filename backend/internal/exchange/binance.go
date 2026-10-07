// Package exchange contains adapters to external venues. The Binance
// collector keeps a long-lived websocket per (symbol, interval) stream
// and publishes every kline update to the in-process event bus. It is
// designed to never silently die:
//
//   - hard read deadline + pong handler detects half-open TCP within
//     ~3 minutes (one missed ping cycle), triggering reconnect
//   - dedicated write pump sends application-level pings every 60s
//   - all goroutines have panic recovery and log+respawn behaviour
//   - exponential backoff with jitter on reconnect, capped at 60s
//   - context cancellation is honoured everywhere
package exchange

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"cryptobot/internal/database/models"
	"cryptobot/internal/eventbus"

	"github.com/gorilla/websocket"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Tunables. Binance sends a ping every ~3 minutes and disconnects after
// 10 minutes without a pong; we play well within those bounds.
const (
	wsHandshakeTimeout = 15 * time.Second
	wsReadTimeout      = 3 * time.Minute  // any silence > this triggers reconnect
	wsWriteTimeout     = 10 * time.Second // per-write deadline
	wsPingInterval     = 60 * time.Second // we proactively ping
	wsBackoffInitial   = 1 * time.Second
	wsBackoffMax       = 60 * time.Second
)

// BinanceCollector connects to Binance kline streams and publishes
// CandleEvents on the bus.
type BinanceCollector struct {
	wsURL string
	bus   *eventbus.Bus

	// OnReconnect, if set, fires once per successful dial that is NOT
	// the first attempt for a given (symbol, interval) stream. It runs
	// in its own goroutine with panic recovery so it can never delay
	// the read loop. Wired by main.go to a GapDetector.
	OnReconnect func(ctx context.Context, symbol, interval string)

	connectedAt   atomic.Int64
	messagesTotal atomic.Uint64
	reconnects    atomic.Uint64
	lastError     atomic.Pointer[string]
}

func NewBinanceCollector(wsURL string, bus *eventbus.Bus) *BinanceCollector {
	return &BinanceCollector{wsURL: wsURL, bus: bus}
}

// Health returns a snapshot of collector health for the /health endpoint.
func (bc *BinanceCollector) Health() map[string]any {
	var lastErr string
	if p := bc.lastError.Load(); p != nil {
		lastErr = *p
	}
	connectedAt := bc.connectedAt.Load()
	uptime := time.Duration(0)
	if connectedAt > 0 {
		uptime = time.Since(time.Unix(0, connectedAt))
	}
	return map[string]any{
		"messages_total":   bc.messagesTotal.Load(),
		"reconnects_total": bc.reconnects.Load(),
		"current_uptime_s": uptime.Seconds(),
		"last_error":       lastErr,
	}
}

// flexString accepts either a JSON string ("123.45") or a JSON number
// (123.45). Binance occasionally switches representation on some
// streams, so we tolerate both to keep ingestion resilient.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if b[0] == '"' && b[len(b)-1] == '"' {
		*f = flexString(b[1 : len(b)-1])
		return nil
	}
	*f = flexString(b)
	return nil
}

func (f flexString) String() string { return string(f) }

type binanceKline struct {
	EventType string `json:"e"`
	EventTime int64  `json:"E"`
	Symbol    string `json:"s"`
	Kline     struct {
		StartTime int64      `json:"t"`
		CloseTime int64      `json:"T"`
		Symbol    string     `json:"s"`
		Interval  string     `json:"i"`
		Open      flexString `json:"o"`
		Close     flexString `json:"c"`
		High      flexString `json:"h"`
		Low       flexString `json:"l"`
		Volume    flexString `json:"v"`
		IsClosed  bool       `json:"x"`
	} `json:"k"`
}

// StreamKlines runs forever (until ctx cancelled), keeping a single
// websocket open for one (symbol, interval). Reconnects with exponential
// backoff on any failure. Recovers from panics. Safe to launch in a
// goroutine — it is its own supervisor.
func (bc *BinanceCollector) StreamKlines(ctx context.Context, symbol, interval string) {
	tag := fmt.Sprintf("%s@%s", strings.ToUpper(symbol), interval)
	url := fmt.Sprintf("%s/%s@kline_%s", bc.wsURL, strings.ToLower(symbol), interval)

	attempt := 0
	backoff := wsBackoffInitial
	for {
		if ctx.Err() != nil {
			log.Printf("[binance %s] context cancelled, stopping stream", tag)
			return
		}

		attempt++
		ok := bc.runOnce(ctx, tag, url, symbol, interval, attempt > 1)
		if ok {
			backoff = wsBackoffInitial
		} else {
			bc.reconnects.Add(1)
			// Backoff with jitter (+/- 30%) to avoid thundering herd
			// when many streams reconnect simultaneously.
			jitter := time.Duration(rand.Int63n(int64(backoff)/3 + 1))
			if rand.Intn(2) == 0 {
				jitter = -jitter
			}
			wait := backoff + jitter
			if wait < 0 {
				wait = backoff
			}
			log.Printf("[binance %s] reconnecting in %s (backoff=%s)", tag, wait, backoff)
			if !sleepCtx(ctx, wait) {
				return
			}
			if backoff < wsBackoffMax {
				backoff *= 2
				if backoff > wsBackoffMax {
					backoff = wsBackoffMax
				}
			}
		}
	}
}

// runOnce manages exactly one websocket connection from dial to close.
// Returns true if the connection ran cleanly to ctx-cancellation,
// false on any error (caller will reconnect).
//
// When isReconnect is true and OnReconnect is set, the callback fires
// in its own goroutine immediately after the WS handshake succeeds —
// so a gap-fill backfill can run in parallel with the resumed stream.
func (bc *BinanceCollector) runOnce(parentCtx context.Context, tag, url, symbol, interval string, isReconnect bool) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			msg := fmt.Sprintf("panic: %v", r)
			bc.recordErr(msg)
			log.Printf("[binance %s] %s", tag, msg)
			ok = false
		}
	}()

	log.Printf("[binance %s] dialing %s", tag, url)
	dialer := websocket.Dialer{HandshakeTimeout: wsHandshakeTimeout}
	dialCtx, cancelDial := context.WithTimeout(parentCtx, wsHandshakeTimeout)
	conn, _, err := dialer.DialContext(dialCtx, url, nil)
	cancelDial()
	if err != nil {
		bc.recordErr("dial: " + err.Error())
		log.Printf("[binance %s] dial error: %v", tag, err)
		return false
	}
	defer conn.Close()

	bc.connectedAt.Store(time.Now().UnixNano())
	log.Printf("[binance %s] connected", tag)

	if isReconnect && bc.OnReconnect != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[binance %s] OnReconnect panic: %v", tag, r)
				}
			}()
			bc.OnReconnect(parentCtx, symbol, interval)
		}()
	}

	if err := conn.SetReadDeadline(time.Now().Add(wsReadTimeout)); err != nil {
		bc.recordErr("set read deadline: " + err.Error())
		return false
	}
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	})
	conn.SetPingHandler(func(appData string) error {
		_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		err := conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(wsWriteTimeout))
		if err == websocket.ErrCloseSent || err == nil {
			return nil
		}
		return err
	})

	// Per-connection context cancelled when read loop dies, so the
	// ping writer also exits and we never leak a goroutine.
	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	go bc.writePump(ctx, tag, conn)

	bc.readLoop(ctx, tag, conn)
	return parentCtx.Err() != nil
}

// writePump is the only goroutine that ever writes to conn — that
// avoids the "concurrent write to websocket connection" panic.
func (bc *BinanceCollector) writePump(ctx context.Context, tag string, conn *websocket.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[binance %s] writePump panic: %v", tag, r)
		}
	}()
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"),
				time.Now().Add(wsWriteTimeout),
			)
			return
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)); err != nil {
				log.Printf("[binance %s] ping write error: %v", tag, err)
				return
			}
		}
	}
}

func (bc *BinanceCollector) readLoop(ctx context.Context, tag string, conn *websocket.Conn) {
	for {
		if ctx.Err() != nil {
			return
		}
		_, msg, err := conn.ReadMessage()
		if err != nil {
			bc.recordErr("read: " + err.Error())
			log.Printf("[binance %s] read error: %v", tag, err)
			return
		}

		// Refresh deadline on ANY data — it counts as keepalive too.
		_ = conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
		bc.messagesTotal.Add(1)

		var event binanceKline
		if err := json.Unmarshal(msg, &event); err != nil {
			log.Printf("[binance %s] decode error: %v", tag, err)
			continue
		}
		if event.Kline.Symbol == "" {
			continue
		}

		candle := models.Candle{
			Symbol:    event.Kline.Symbol,
			Interval:  event.Kline.Interval,
			OpenTime:  time.UnixMilli(event.Kline.StartTime),
			CloseTime: time.UnixMilli(event.Kline.CloseTime),
			Open:      toDecimal(event.Kline.Open.String()),
			High:      toDecimal(event.Kline.High.String()),
			Low:       toDecimal(event.Kline.Low.String()),
			Close:     toDecimal(event.Kline.Close.String()),
			Volume:    toDecimal(event.Kline.Volume.String()),
		}

		evt := eventbus.CandleEvent{
			Candle:   candle,
			IsClosed: event.Kline.IsClosed,
			RecvAt:   time.Now(),
		}

		// Always emit the live tick for charts.
		bc.bus.Publish(eventbus.TopicCandleTick, evt)
		// Persister only cares about closed candles.
		if event.Kline.IsClosed {
			bc.bus.Publish(eventbus.TopicCandleClosed, evt)
		}
	}
}

func (bc *BinanceCollector) recordErr(s string) { bc.lastError.Store(&s) }

func toDecimal(s string) decimal.Decimal {
	d, _ := decimal.NewFromString(s)
	return d
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// ─────────────────────────────────────────────────────────────────────
// Historical backfill (REST). Used for startup hydration only.
// ─────────────────────────────────────────────────────────────────────

func FetchHistoricalCandles(restURL, symbol, interval string, limit int) ([]models.Candle, error) {
	if limit > 1000 {
		limit = 1000
	}
	url := fmt.Sprintf("%s/api/v3/klines?symbol=%s&interval=%s&limit=%d", restURL, symbol, interval, limit)
	return fetchKlines(httpClient(), url, symbol, interval)
}

// klineIntervalMs maps Binance interval strings to milliseconds. Used by
// FetchHistoricalCandlesRange to advance the pagination cursor by exactly
// one candle. "1M" (calendar month) and other variable-length intervals are
// intentionally unsupported — backfill is built for fixed-stride buckets.
func klineIntervalMs(interval string) (int64, error) {
	switch interval {
	case "1m":
		return 60_000, nil
	case "3m":
		return 3 * 60_000, nil
	case "5m":
		return 5 * 60_000, nil
	case "15m":
		return 15 * 60_000, nil
	case "30m":
		return 30 * 60_000, nil
	case "1h":
		return 60 * 60_000, nil
	case "2h":
		return 2 * 60 * 60_000, nil
	case "4h":
		return 4 * 60 * 60_000, nil
	case "6h":
		return 6 * 60 * 60_000, nil
	case "8h":
		return 8 * 60 * 60_000, nil
	case "12h":
		return 12 * 60 * 60_000, nil
	case "1d":
		return 24 * 60 * 60_000, nil
	case "3d":
		return 3 * 24 * 60 * 60_000, nil
	case "1w":
		return 7 * 24 * 60 * 60_000, nil
	}
	return 0, fmt.Errorf("unsupported backfill interval %q", interval)
}

// rangeSleep is the gap between paginated /klines calls. Binance grants
// /klines weight=1 inside a 6000-weight/minute IP budget; 200ms (5 req/s
// = 300 req/min) stays an order of magnitude clear of the cap and matches
// the CRY-8 spec. Exposed as a var (not const) so tests can shorten it.
var rangeSleep = 200 * time.Millisecond

// httpClient is a hook so tests can supply a stub. Default to a 15s timeout
// matching the single-page fetcher.
var httpClient = func() *http.Client { return &http.Client{Timeout: 15 * time.Second} }

// FetchHistoricalCandlesRange paginates /api/v3/klines to cover [start, end].
// Returns candles in chronological order, deduped on Binance's openTime.
// Idempotent at the row level via the (symbol, interval, open_time) unique
// index downstream — callers feed the result to UpsertCandles.
//
// Binance returns up to 1000 candles per call; we advance the cursor by
// (last openTime + interval) and sleep 200ms between calls to stay well
// inside the rate-limit budget. A short page (< 1000 rows) signals end of
// data and we stop. A defensive cursor-non-advance check breaks rather
// than loops forever if the venue ever returns out-of-order pages.
func FetchHistoricalCandlesRange(restURL, symbol, interval string, start, end time.Time) ([]models.Candle, error) {
	intervalMs, err := klineIntervalMs(interval)
	if err != nil {
		return nil, err
	}
	if !end.After(start) {
		return nil, fmt.Errorf("backfill range invalid: end %s not after start %s", end, start)
	}

	client := httpClient()
	const pageLimit = 1000
	cursor := start.UnixMilli()
	endMs := end.UnixMilli()
	out := make([]models.Candle, 0, 1024)

	for call := 0; cursor <= endMs; call++ {
		if call > 0 {
			time.Sleep(rangeSleep)
		}
		url := fmt.Sprintf(
			"%s/api/v3/klines?symbol=%s&interval=%s&startTime=%d&endTime=%d&limit=%d",
			restURL, symbol, interval, cursor, endMs, pageLimit,
		)
		page, err := fetchKlines(client, url, symbol, interval)
		if err != nil {
			return nil, fmt.Errorf("kline page (cursor=%d): %w", cursor, err)
		}
		if len(page) == 0 {
			break
		}
		out = append(out, page...)
		// Bump cursor to the next bucket so the same candle isn't refetched
		// (Binance's startTime is inclusive). If the venue ever fails to
		// advance — out-of-order page, clock skew — bail rather than loop.
		next := page[len(page)-1].OpenTime.UnixMilli() + intervalMs
		if next <= cursor {
			break
		}
		cursor = next
		if len(page) < pageLimit {
			break
		}
	}
	return out, nil
}

// fetchKlines GETs and parses a single /api/v3/klines response. The
// goofy 12-field tuple-array shape is Binance's wire format; we map only
// the fields the candles table holds.
func fetchKlines(client *http.Client, url, symbol, interval string) ([]models.Candle, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("binance REST request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("binance REST %d: %s", resp.StatusCode, string(body))
	}

	var raw [][]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode klines: %w", err)
	}

	candles := make([]models.Candle, 0, len(raw))
	for _, k := range raw {
		if len(k) < 12 {
			continue
		}
		var openTime, closeTime int64
		var openS, highS, lowS, closeS, volS string
		_ = json.Unmarshal(k[0], &openTime)
		_ = json.Unmarshal(k[1], &openS)
		_ = json.Unmarshal(k[2], &highS)
		_ = json.Unmarshal(k[3], &lowS)
		_ = json.Unmarshal(k[4], &closeS)
		_ = json.Unmarshal(k[5], &volS)
		_ = json.Unmarshal(k[6], &closeTime)

		candles = append(candles, models.Candle{
			Symbol:    symbol,
			Interval:  interval,
			OpenTime:  time.UnixMilli(openTime),
			CloseTime: time.UnixMilli(closeTime),
			Open:      toDecimal(openS),
			High:      toDecimal(highS),
			Low:       toDecimal(lowS),
			Close:     toDecimal(closeS),
			Volume:    toDecimal(volS),
		})
	}
	return candles, nil
}

// UpsertCandles writes a candle slice using the (symbol, interval, open_time)
// unique index. ON CONFLICT updates OHLCV so REST history is authoritative:
// a stale WS partial gets overwritten by the closed REST candle. Idempotent —
// re-running converges every row to the latest exchange snapshot.
func UpsertCandles(db *gorm.DB, candles []models.Candle) (int, error) {
	if len(candles) == 0 {
		return 0, nil
	}
	res := db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "interval"}, {Name: "open_time"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"close_time", "open", "high", "low", "close", "volume",
		}),
	}).CreateInBatches(&candles, 200)
	if res.Error != nil {
		return 0, res.Error
	}
	return int(res.RowsAffected), nil
}

// Backfill fetches a single page (≤ 1000 rows) of recent history and
// upserts it. Used by startup hydration; cmd/backfill drives the range-
// based path instead.
func Backfill(db *gorm.DB, restURL, symbol, interval string, limit int) (int, error) {
	candles, err := FetchHistoricalCandles(restURL, symbol, interval, limit)
	if err != nil {
		return 0, err
	}
	return UpsertCandles(db, candles)
}

func ParseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
