// Package ingestion — GapDetector fills gaps that may have accumulated
// in the candles table while a stream was disconnected. It is invoked
// after a successful WS reconnect (not on cold start — main.go's bulk
// backfill owns that path).
//
// Idempotent: relies on exchange.Backfill, which UPSERTs on the
// (symbol, interval, open_time) unique index. Running concurrently or
// repeatedly cannot create duplicates.
package ingestion

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"cryptobot/internal/database/models"
	"cryptobot/internal/exchange"

	"gorm.io/gorm"
)

// Hard limits.
const (
	defaultMaxLookback = 24 * time.Hour // never REST-backfill further than this
	binanceKlineLimit  = 1000           // Binance REST kline limit
)

// GapDetector inspects the candle table and decides whether a REST
// backfill is needed to fill a gap. Construct one per process; safe to
// call FillIfGap from multiple goroutines.
type GapDetector struct {
	db          *gorm.DB
	restURL     string
	maxLookback time.Duration
}

func NewGapDetector(db *gorm.DB, restURL string, maxLookback time.Duration) *GapDetector {
	if maxLookback <= 0 {
		maxLookback = defaultMaxLookback
	}
	return &GapDetector{db: db, restURL: restURL, maxLookback: maxLookback}
}

// FillIfGap queries the latest candle for (symbol, interval) and
// REST-backfills if the elapsed time since that candle exceeds two
// intervals. No-ops when the DB has no row yet (cold start case).
func (g *GapDetector) FillIfGap(ctx context.Context, symbol, interval string) {
	iv, err := ParseInterval(interval)
	if err != nil {
		log.Printf("[gapdetector] sym=%s iv=%s unsupported interval: %v", symbol, interval, err)
		return
	}

	var last models.Candle
	err = g.db.WithContext(ctx).
		Where("symbol = ? AND interval = ?", symbol, interval).
		Order("open_time DESC").
		Limit(1).
		Take(&last).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Printf("[gapdetector] sym=%s iv=%s query err: %v", symbol, interval, err)
		}
		return
	}

	n := ComputeBackfillN(last.OpenTime, time.Now(), iv, g.maxLookback)
	if n == 0 {
		return
	}

	filled, err := exchange.Backfill(g.db, g.restURL, symbol, interval, n)
	if err != nil {
		log.Printf("[gapdetector] sym=%s iv=%s backfill err: %v", symbol, interval, err)
		return
	}
	log.Printf("[gapdetector] sym=%s iv=%s last=%s gap=%s requested=%d filled=%d",
		symbol, interval,
		last.OpenTime.UTC().Format(time.RFC3339),
		time.Since(last.OpenTime).Round(time.Second),
		n, filled,
	)
}

// ComputeBackfillN is the pure decision function — exported for tests.
// Returns 0 when no meaningful gap exists (elapsed < 2*interval), else
// the number of candles to request via REST, capped at the Binance
// kline limit and the configured maxLookback window.
func ComputeBackfillN(lastOpenTime, now time.Time, interval, maxLookback time.Duration) int {
	if interval <= 0 {
		return 0
	}
	elapsed := now.Sub(lastOpenTime)
	if elapsed < 2*interval {
		return 0
	}
	if elapsed > maxLookback {
		elapsed = maxLookback
	}
	// +1 to cover the partially-open current bucket; Binance returns the
	// closest N candles up to and including the open one.
	n := int(elapsed/interval) + 1
	if n > binanceKlineLimit {
		n = binanceKlineLimit
	}
	return n
}

// ParseInterval converts a Binance kline interval string into a duration.
// Exported so callers (and tests) can reuse without re-implementing.
func ParseInterval(s string) (time.Duration, error) {
	switch s {
	case "1m":
		return time.Minute, nil
	case "3m":
		return 3 * time.Minute, nil
	case "5m":
		return 5 * time.Minute, nil
	case "15m":
		return 15 * time.Minute, nil
	case "30m":
		return 30 * time.Minute, nil
	case "1h":
		return time.Hour, nil
	case "2h":
		return 2 * time.Hour, nil
	case "4h":
		return 4 * time.Hour, nil
	case "6h":
		return 6 * time.Hour, nil
	case "12h":
		return 12 * time.Hour, nil
	case "1d":
		return 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("unsupported interval %q", s)
	}
}
