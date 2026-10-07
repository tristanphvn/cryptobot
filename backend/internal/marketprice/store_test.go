package marketprice

import (
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestStore_GetMiss(t *testing.T) {
	s := New()
	if _, ok := s.Get("BTCUSDT"); ok {
		t.Fatal("expected miss for never-set symbol")
	}
}

func TestStore_SetGet(t *testing.T) {
	s := New()
	now := time.Now()
	s.Set("BTCUSDT", decimal.NewFromInt(60000), now)
	q, ok := s.Get("BTCUSDT")
	if !ok {
		t.Fatal("expected hit")
	}
	if !q.Price.Equal(decimal.NewFromInt(60000)) {
		t.Fatalf("price mismatch: got %s", q.Price)
	}
	if !q.At.Equal(now) {
		t.Fatalf("timestamp mismatch: got %v want %v", q.At, now)
	}
}

func TestStore_SetIgnoresEmptyAndZero(t *testing.T) {
	s := New()
	s.Set("", decimal.NewFromInt(1), time.Now())
	if _, ok := s.Get(""); ok {
		t.Fatal("expected empty symbol to be rejected")
	}
	s.Set("ETHUSDT", decimal.Zero, time.Now())
	if _, ok := s.Get("ETHUSDT"); ok {
		t.Fatal("expected zero price to be rejected")
	}
}

func TestQuote_IsStale(t *testing.T) {
	q := Quote{Price: decimal.NewFromInt(1), At: time.Now().Add(-30 * time.Second)}
	if !q.IsStale(10 * time.Second) {
		t.Fatal("expected stale=true for 30s-old quote with 10s maxAge")
	}
	if q.IsStale(time.Minute) {
		t.Fatal("expected stale=false for 30s-old quote with 1m maxAge")
	}
}

// TestStore_ConcurrentSetGet runs many writers and readers in parallel
// to surface any race when the -race flag is enabled. Pure correctness
// test — no assertion on final values, the race detector is the assertion.
func TestStore_ConcurrentSetGet(t *testing.T) {
	s := New()
	const writers = 8
	const readers = 8
	const iters = 1000
	var wg sync.WaitGroup
	wg.Add(writers + readers)
	for range writers {
		go func() {
			defer wg.Done()
			for i := range iters {
				s.Set("BTCUSDT", decimal.NewFromInt(int64(60000+i)), time.Now())
			}
		}()
	}
	for range readers {
		go func() {
			defer wg.Done()
			for range iters {
				_, _ = s.Get("BTCUSDT")
			}
		}()
	}
	wg.Wait()
}

func TestStore_Snapshot(t *testing.T) {
	s := New()
	s.Set("BTCUSDT", decimal.NewFromInt(60000), time.Now())
	s.Set("ETHUSDT", decimal.NewFromInt(3000), time.Now())
	snap := s.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2", len(snap))
	}
	// mutating snapshot must not affect store
	delete(snap, "BTCUSDT")
	if _, ok := s.Get("BTCUSDT"); !ok {
		t.Fatal("snapshot mutation leaked back into store")
	}
}
