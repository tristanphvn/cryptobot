// Package marketprice is the in-memory single-source-of-truth for the
// freshest known market price per symbol. It exists so the risk engine
// (position monitor) and analyzer never have to round-trip the database
// to learn what something costs right now.
//
// Why not just read the latest candle row?
//   - The persister batches inserts every ~2s for throughput; a row
//     visible to SELECT is already at least one tick behind the wire.
//   - A risk engine evaluating SL/TP MUST evaluate against the freshest
//     price the runtime knows, not a delayed persistence snapshot.
//
// Architecture:
//   - One Store per process. main.go constructs it once and hands it
//     to anyone who needs Set or Get.
//   - Set is called by the bus consumer Run() on every TopicCandleTick
//     event (every WS update, not just closed candles).
//   - Get is called by the position monitor on every tick and by the
//     analyzer when building MarketContext.Position.
//
// Concurrency: a single sync.RWMutex protects the map. Set takes the
// write lock; Get takes the read lock. The map only ever grows (one
// entry per symbol the bot watches) so there is no eviction logic to
// coordinate.
package marketprice

import (
	"context"
	"log"
	"sync"
	"time"

	"cryptobot/internal/eventbus"

	"github.com/shopspring/decimal"
)

// Quote is what Get returns. Price is the freshest known close; At is
// the wall-clock time Set was called (NOT the candle's exchange-side
// timestamp — we care about how stale the runtime's belief is, not
// when the candle started).
type Quote struct {
	Price decimal.Decimal
	At    time.Time
}

// IsStale reports whether the quote is older than maxAge. Callers that
// must not act on stale data (the position monitor) check this before
// trusting Price.
func (q Quote) IsStale(maxAge time.Duration) bool {
	return time.Since(q.At) > maxAge
}

// Store is the goroutine-safe map. Construct with New().
type Store struct {
	mu     sync.RWMutex
	quotes map[string]Quote
}

func New() *Store {
	return &Store{quotes: make(map[string]Quote)}
}

// Set updates the quote for symbol. Called by the bus consumer on every
// TopicCandleTick event. Symbol is stored exactly as received (already
// uppercase from the exchange) so callers can pass the same string they
// got from configuration.
func (s *Store) Set(symbol string, price decimal.Decimal, at time.Time) {
	if symbol == "" || price.IsZero() {
		return
	}
	s.mu.Lock()
	s.quotes[symbol] = Quote{Price: price, At: at}
	s.mu.Unlock()
}

// Get returns the freshest known quote for symbol. ok=false means the
// runtime has never seen a tick for this symbol — caller MUST treat
// this as "no information" and skip any decision that needs a price.
func (s *Store) Get(symbol string) (Quote, bool) {
	s.mu.RLock()
	q, ok := s.quotes[symbol]
	s.mu.RUnlock()
	return q, ok
}

// Snapshot returns a shallow copy of the entire map for diagnostics
// (used by /health). Held lock duration is one map iteration.
func (s *Store) Snapshot() map[string]Quote {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Quote, len(s.quotes))
	for k, v := range s.quotes {
		out[k] = v
	}
	return out
}

// Run subscribes to TopicCandleTick and forwards every event into Set.
// Blocks until ctx is cancelled. Designed to be launched via supervise()
// — a panic recovered here means a malformed event slipped through, and
// the supervisor will restart Run with a small backoff.
//
// Buffer 1024 is generous: at minute boundaries we can see ~50 events
// in a few ms (8 symbols * 6 intervals). A slow consumer would drop
// events at the bus level (logged by eventbus.Bus), not stall the
// publisher.
func (s *Store) Run(ctx context.Context, bus *eventbus.Bus) {
	sub := bus.Subscribe(eventbus.TopicCandleTick, 1024, "marketprice")
	defer sub.Close()

	log.Println("[marketprice] started — subscribed to candle.tick")

	for {
		select {
		case <-ctx.Done():
			log.Println("[marketprice] stopped")
			return
		case raw, ok := <-sub.C:
			if !ok {
				return
			}
			evt, ok := raw.(eventbus.CandleEvent)
			if !ok {
				continue
			}
			s.Set(evt.Candle.Symbol, evt.Candle.Close, evt.RecvAt)
		}
	}
}
