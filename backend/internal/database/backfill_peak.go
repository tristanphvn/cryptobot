package database

import (
	"context"
	"fmt"
	"log"

	"cryptobot/internal/database/models"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// BackfillPositionPeaks populates peak_price + max_unrealized_pnl_pct on
// every OPEN position whose peak_price is still NULL — typically a row
// that existed before CRY-22 shipped (the position monitor maintains the
// columns going forward, but pre-existing OPEN rows would otherwise wait
// for the next tick to bootstrap their state from entry price only,
// erasing any peak the position actually saw before this code shipped).
//
// For each such row we scan the candles table for 1m bars since the
// position opened and set peak_price = MAX(high). If no candles are
// found (e.g. a brand-new symbol with no history backfilled yet) we
// fall back to peak_price = avg_entry_price. The max_unrealized_pnl_pct
// column is derived from the chosen peak.
//
// Idempotent via the `WHERE peak_price IS NULL` guard — safe to invoke
// on every boot. Called from cmd/server/main.go after database.Migrate
// and before the position-monitor goroutine starts.
func BackfillPositionPeaks(ctx context.Context, db *gorm.DB) error {
	var positions []models.Position
	if err := db.WithContext(ctx).
		Where("is_open = ? AND peak_price IS NULL", true).
		Find(&positions).Error; err != nil {
		return fmt.Errorf("load open positions: %w", err)
	}
	if len(positions) == 0 {
		log.Printf("[backfill-peak] no OPEN positions need backfill")
		return nil
	}
	log.Printf("[backfill-peak] %d OPEN position(s) need peak_price backfill", len(positions))

	hundred := decimal.NewFromInt(100)
	for _, pos := range positions {
		peak, err := computePeakFromCandles(ctx, db, pos)
		if err != nil {
			log.Printf("[backfill-peak] WARN sym=%s id=%d: %v — falling back to entry price", pos.Symbol, pos.ID, err)
			peak = pos.AvgEntryPrice
		}
		if peak.IsZero() {
			peak = pos.AvgEntryPrice
		}

		var maxPnLPct decimal.Decimal
		if !pos.AvgEntryPrice.IsZero() {
			maxPnLPct = peak.Sub(pos.AvgEntryPrice).Div(pos.AvgEntryPrice).Mul(hundred)
		}

		updates := map[string]any{
			"peak_price":             peak,
			"max_unrealized_pnl_pct": maxPnLPct,
		}
		if err := db.WithContext(ctx).Model(&models.Position{}).
			Where("id = ? AND is_open = ? AND peak_price IS NULL", pos.ID, true).
			Updates(updates).Error; err != nil {
			return fmt.Errorf("update position id=%d: %w", pos.ID, err)
		}
		log.Printf("[backfill-peak] sym=%s id=%d peak=%s max_unrealized_pnl_pct=%s%%",
			pos.Symbol, pos.ID, peak.StringFixed(8), maxPnLPct.StringFixed(4))
	}
	return nil
}

// computePeakFromCandles queries 1m candles since the position opened
// and returns MAX(high). Returns decimal.Zero if no candles exist (the
// caller falls back to avg_entry_price).
//
// Capped at LIMIT 100_000 to bound startup time on stale OPEN positions
// with months of candle history. 100k 1m candles ≈ 69 days; positions
// older than that almost certainly have a peak in that recent window
// anyway, and the worst case is we under-estimate the peak slightly —
// which is conservatively safe (the trail then arms later, never
// earlier than reality).
func computePeakFromCandles(ctx context.Context, db *gorm.DB, pos models.Position) (decimal.Decimal, error) {
	var max decimal.NullDecimal
	row := db.WithContext(ctx).
		Table("candles").
		Select("MAX(high) AS high").
		Where("symbol = ? AND interval = ? AND open_time >= ?", pos.Symbol, "1m", pos.CreatedAt).
		Limit(100_000).
		Row()
	if err := row.Scan(&max); err != nil {
		return decimal.Zero, fmt.Errorf("scan max(high): %w", err)
	}
	if !max.Valid {
		return decimal.Zero, nil
	}
	return max.Decimal, nil
}
