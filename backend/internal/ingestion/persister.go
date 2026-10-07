// Package ingestion contains background workers that consume events
// from the bus and write them to durable storage. Workers are designed
// to never block publishers and to survive transient DB errors with
// retries + bounded backoff.
package ingestion

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"cryptobot/internal/database/models"
	"cryptobot/internal/eventbus"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Persister batches closed candles and UPSERTs them into TimescaleDB.
// Batching dramatically reduces DB roundtrips at minute boundaries
// where many symbol/interval streams close their candle simultaneously.
type Persister struct {
	db        *gorm.DB
	bus       *eventbus.Bus
	batchSize int
	flushEvry time.Duration

	// writeFn is the actual storage path. Defaults to p.write (the
	// gorm UPSERT). Tests inject a fake to verify batching behaviour
	// without spinning up a real database.
	writeFn func(ctx context.Context, batch []models.Candle) error

	written  atomic.Uint64
	failed   atomic.Uint64
	lastWrAt atomic.Int64
}

func NewPersister(db *gorm.DB, bus *eventbus.Bus, batchSize int, flushEvery time.Duration) *Persister {
	if batchSize <= 0 {
		batchSize = 50
	}
	if flushEvery <= 0 {
		flushEvery = 2 * time.Second
	}
	p := &Persister{db: db, bus: bus, batchSize: batchSize, flushEvry: flushEvery}
	p.writeFn = p.write
	return p
}

// Health returns counters for diagnostics.
func (p *Persister) Health() map[string]any {
	last := p.lastWrAt.Load()
	var lastWrAt any
	if last > 0 {
		lastWrAt = time.Unix(0, last).UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"candles_written": p.written.Load(),
		"write_failures":  p.failed.Load(),
		"last_write_at":   lastWrAt,
	}
}

// Run blocks until ctx is cancelled. Launch in a goroutine.
// Crashes (panics) are recovered and the loop is restarted with a
// short delay so a single bad event can never kill ingestion forever.
func (p *Persister) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		p.runOnce(ctx)
		// runOnce only returns on panic or ctx done; small cooldown.
		if ctx.Err() != nil {
			return
		}
		log.Println("[persister] restarting after unexpected exit")
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (p *Persister) runOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[persister] panic recovered: %v", r)
		}
	}()

	// Buffer is generous: at minute boundaries we may receive several
	// dozen events in a few milliseconds across all symbols.
	sub := p.bus.Subscribe(eventbus.TopicCandleClosed, 256, "persister")
	defer sub.Close()

	buf := make([]models.Candle, 0, p.batchSize)
	ticker := time.NewTicker(p.flushEvry)
	defer ticker.Stop()

	flush := func(reason string) {
		if len(buf) == 0 {
			return
		}
		if err := p.writeFn(ctx, buf); err != nil {
			p.failed.Add(uint64(len(buf)))
			log.Printf("[persister] write failed (%s, n=%d): %v", reason, len(buf), err)
		} else {
			p.written.Add(uint64(len(buf)))
			p.lastWrAt.Store(time.Now().UnixNano())
			log.Printf("[persister] wrote batch n=%d reason=%s", len(buf), reason)
		}
		buf = buf[:0]
	}

	log.Printf("[persister] started — batch_size=%d flush_every=%s", p.batchSize, p.flushEvry)

	for {
		select {
		case <-ctx.Done():
			flush("shutdown")
			log.Println("[persister] stopped")
			return
		case <-ticker.C:
			flush("tick")
		case raw, ok := <-sub.C:
			if !ok {
				flush("bus_closed")
				return
			}
			evt, ok := raw.(eventbus.CandleEvent)
			if !ok {
				continue
			}
			buf = append(buf, evt.Candle)
			if len(buf) >= p.batchSize {
				flush("full")
			}
		}
	}
}

// write performs a single UPSERT batch with a few retries on transient errors.
func (p *Persister) write(ctx context.Context, batch []models.Candle) error {
	const maxAttempts = 3
	var lastErr error
	delay := 100 * time.Millisecond
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Copy to a fresh slice to avoid GORM mutating the caller's buffer.
		toWrite := make([]models.Candle, len(batch))
		copy(toWrite, batch)

		err := p.db.WithContext(ctx).
			Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "symbol"}, {Name: "interval"}, {Name: "open_time"}},
				DoNothing: true,
			}).
			CreateInBatches(&toWrite, 100).Error
		if err == nil {
			return nil
		}
		lastErr = err
		log.Printf("[persister] attempt %d/%d failed: %v", attempt, maxAttempts, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return lastErr
}
