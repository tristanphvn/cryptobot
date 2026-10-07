package ingestion

import (
	"context"
	"sync"
	"testing"
	"time"

	"cryptobot/internal/database/models"
	"cryptobot/internal/eventbus"
)

// captureWriter is a thread-safe recorder used in place of the real
// gorm-backed writer so persister tests don't need a database.
type captureWriter struct {
	mu      sync.Mutex
	batches [][]models.Candle
	err     error // when set, write returns err and does not record
}

func (c *captureWriter) write(_ context.Context, batch []models.Candle) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	cp := make([]models.Candle, len(batch))
	copy(cp, batch)
	c.batches = append(c.batches, cp)
	return nil
}

func (c *captureWriter) snapshot() [][]models.Candle {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([][]models.Candle, len(c.batches))
	copy(out, c.batches)
	return out
}

func newTestPersister(bus *eventbus.Bus, batchSize int, flushEvery time.Duration) (*Persister, *captureWriter) {
	w := &captureWriter{}
	p := NewPersister(nil, bus, batchSize, flushEvery)
	p.writeFn = w.write
	return p, w
}

func sampleEvent(sym string) eventbus.CandleEvent {
	return eventbus.CandleEvent{
		Candle: models.Candle{
			Symbol:   sym,
			Interval: "1m",
			OpenTime: time.Now(),
		},
		IsClosed: true,
		RecvAt:   time.Now(),
	}
}

func TestPersisterFlushesWhenBatchFull(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()

	p, w := newTestPersister(bus, 3, time.Hour) // long flush — only size triggers
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Run(ctx)
	time.Sleep(50 * time.Millisecond) // wait for subscribe

	for i := 0; i < 5; i++ {
		bus.Publish(eventbus.TopicCandleClosed, sampleEvent("BTCUSDT"))
	}

	// Give consumer time to drain
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.snapshot()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	got := w.snapshot()
	if len(got) == 0 {
		t.Fatal("expected at least one flushed batch, got none")
	}
	if len(got[0]) != 3 {
		t.Fatalf("expected first batch size=3 (full trigger), got %d", len(got[0]))
	}
}

func TestPersisterFlushesOnTicker(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()

	// Large batch + short flush — only ticker can trigger
	p, w := newTestPersister(bus, 1000, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 2; i++ {
		bus.Publish(eventbus.TopicCandleClosed, sampleEvent("ETHUSDT"))
	}

	// Wait for one tick.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(w.snapshot()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	got := w.snapshot()
	if len(got) == 0 {
		t.Fatal("ticker did not trigger flush within 2s")
	}
	if len(got[0]) != 2 {
		t.Fatalf("expected ticker batch size=2, got %d", len(got[0]))
	}
}

func TestPersisterCtxCancelFlushesPending(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()

	p, w := newTestPersister(bus, 1000, time.Hour) // neither size nor tick fire
	ctx, cancel := context.WithCancel(context.Background())

	go p.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 3; i++ {
		bus.Publish(eventbus.TopicCandleClosed, sampleEvent("SOLUSDT"))
	}
	time.Sleep(50 * time.Millisecond) // let events queue

	cancel()
	time.Sleep(100 * time.Millisecond) // let shutdown flush

	got := w.snapshot()
	if len(got) != 1 || len(got[0]) != 3 {
		t.Fatalf("expected one shutdown flush of size=3, got %v", got)
	}
}

func TestPersisterIgnoresWrongPayloadType(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()

	p, w := newTestPersister(bus, 1000, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go p.Run(ctx)
	time.Sleep(50 * time.Millisecond)

	bus.Publish(eventbus.TopicCandleClosed, "not-a-candle")
	bus.Publish(eventbus.TopicCandleClosed, sampleEvent("XRPUSDT"))

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) && len(w.snapshot()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	got := w.snapshot()
	if len(got) == 0 {
		t.Fatal("expected one batch, got none")
	}
	if len(got[0]) != 1 {
		t.Fatalf("expected batch size=1 (bad payload dropped), got %d", len(got[0]))
	}
}
