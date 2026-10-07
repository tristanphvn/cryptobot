// Package retention is the database's data lifecycle (CRY-38): how long
// each kind of row is kept, in what form. It is the single source for
// pruning — the server runs it on a schedule and cmd/prune runs it once.
//
// Measured before this existed (2026-10-06, local DB): candles were 82%
// of the database and nothing ever removed them, while the old manual
// prune.sql deleted whole audit rows after 7 days — the structured
// columns later forensics (CRY-23 / CRY-36) depend on.
package retention

import (
	"context"
	"fmt"
	"log"
	"time"

	"cryptobot/internal/config"

	"gorm.io/gorm"
)

// Config holds the retention windows in days. 0 disables a rule.
type Config struct {
	// Candle1mDays keeps 1m candles (dashboard 1H–24H charts, position
	// peak backfill). Timescale prunes whole chunks, so rows can live up
	// to one chunk interval (7 days) longer than this.
	Candle1mDays int
	// CandleIntradayDays keeps 5m / 15m candles. 1h / 4h / 1d are kept
	// indefinitely: they are small and feed the backtester.
	CandleIntradayDays int
	// AuditTextDays clears audit_logs.prompt / raw_response after this
	// many days. The row and its structured columns are kept.
	AuditTextDays int
	// FailedPendingDays deletes expired / rejected / failed pending
	// signals; FailedTradeDays deletes FAILED trade rows.
	FailedPendingDays int
	FailedTradeDays   int
}

// Report counts what one run removed or cleared.
type Report struct {
	Candles1m       int64
	CandlesIntraday int64
	AuditTextRows   int64
	PendingSignals  int64
	FailedTrades    int64
}

func (r Report) String() string {
	return fmt.Sprintf("candles_1m=%d candles_intraday=%d audit_text=%d pending_signals=%d failed_trades=%d",
		r.Candles1m, r.CandlesIntraday, r.AuditTextRows, r.PendingSignals, r.FailedTrades)
}

// Run applies every enabled rule once. hypertable selects the Timescale
// path for candles (see pruneCandlesChunked).
func Run(ctx context.Context, db *gorm.DB, cfg Config, hypertable bool, now time.Time) (Report, error) {
	var rep Report
	var err error
	db = db.WithContext(ctx)

	if rep.Candles1m, err = pruneCandles(db, []string{"1m"}, cfg.Candle1mDays, hypertable, now); err != nil {
		return rep, fmt.Errorf("candles 1m: %w", err)
	}
	if rep.CandlesIntraday, err = pruneCandles(db, []string{"5m", "15m"}, cfg.CandleIntradayDays, hypertable, now); err != nil {
		return rep, fmt.Errorf("candles intraday: %w", err)
	}
	if cfg.AuditTextDays > 0 {
		res := db.Exec(`UPDATE audit_logs SET prompt = '', raw_response = ''
			WHERE prompt_sent_at < ? AND (prompt <> '' OR raw_response <> '')`, cutoff(now, cfg.AuditTextDays))
		if res.Error != nil {
			return rep, fmt.Errorf("audit text: %w", res.Error)
		}
		rep.AuditTextRows = res.RowsAffected
	}
	if cfg.FailedPendingDays > 0 {
		// Terminal-failure states only: `submitted` is a fired live order
		// and in-flight states must never be touched.
		res := db.Exec(`DELETE FROM pending_signals
			WHERE status IN ('expired', 'rejected', 'failed')
			  AND COALESCE(decided_at, expires_at, created_at) < ?`, cutoff(now, cfg.FailedPendingDays))
		if res.Error != nil {
			return rep, fmt.Errorf("pending signals: %w", res.Error)
		}
		rep.PendingSignals = res.RowsAffected
	}
	if cfg.FailedTradeDays > 0 {
		res := db.Exec(`DELETE FROM trades WHERE status = 'FAILED' AND created_at < ?`, cutoff(now, cfg.FailedTradeDays))
		if res.Error != nil {
			return rep, fmt.Errorf("failed trades: %w", res.Error)
		}
		rep.FailedTrades = res.RowsAffected
	}
	return rep, nil
}

func cutoff(now time.Time, days int) time.Time {
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

func pruneCandles(db *gorm.DB, intervals []string, days int, hypertable bool, now time.Time) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	before := cutoff(now, days)
	if !hypertable {
		res := db.Exec(`DELETE FROM candles WHERE interval IN ? AND open_time < ?`, intervals, before)
		return res.RowsAffected, res.Error
	}
	return pruneCandlesChunked(db, intervals, before)
}

// pruneCandlesChunked deletes old candles from a compressed hypertable.
//
// A plain DELETE on a compressed chunk decompresses the matching rows
// into the chunk's uncompressed heap (capped at 100k tuples per
// transaction by default) and leaves that heap bloated after the rows
// are gone. Instead each chunk lying wholly before the cutoff is handled
// in its own transaction: count first (cheap — interval is a segmentby
// column, so batch metadata answers it), and only when there is
// something to remove decompress → delete → compress. Re-compressing a
// fully decompressed chunk rewrites it compactly. Chunks straddling the
// cutoff wait for a later run, so a row can outlive its window by up to
// one chunk interval.
func pruneCandlesChunked(db *gorm.DB, intervals []string, before time.Time) (int64, error) {
	var chunks []string
	if err := db.Raw(`SELECT show_chunks('candles', older_than => ?::timestamptz)::text`, before).Scan(&chunks).Error; err != nil {
		return 0, err
	}
	var total int64
	for _, chunk := range chunks {
		// chunk is a regclass name returned by show_chunks, not user input.
		var n int64
		if err := db.Raw(fmt.Sprintf(`SELECT count(*) FROM %s WHERE interval IN ?`, chunk), intervals).Scan(&n).Error; err != nil {
			return total, fmt.Errorf("count %s: %w", chunk, err)
		}
		if n == 0 {
			continue
		}
		err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec(`SELECT decompress_chunk(?::regclass, if_compressed => TRUE)`, chunk).Error; err != nil {
				return err
			}
			res := tx.Exec(fmt.Sprintf(`DELETE FROM %s WHERE interval IN ?`, chunk), intervals)
			if res.Error != nil {
				return res.Error
			}
			total += res.RowsAffected
			return tx.Exec(`SELECT compress_chunk(?::regclass, if_not_compressed => TRUE)`, chunk).Error
		})
		if err != nil {
			return total, fmt.Errorf("prune %s: %w", chunk, err)
		}
	}
	return total, nil
}

// Loop runs Run after initialDelay and then every interval until ctx
// ends. Failures are logged and retried on the next tick.
func Loop(ctx context.Context, db *gorm.DB, cfg Config, hypertable bool, initialDelay, interval time.Duration) {
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		start := time.Now()
		rep, err := Run(ctx, db, cfg, hypertable, start)
		if err != nil {
			log.Printf("[retention] run failed after %s: %v (%s)", time.Since(start).Round(time.Millisecond), err, rep)
		} else {
			log.Printf("[retention] run ok in %s: %s", time.Since(start).Round(time.Millisecond), rep)
		}
		timer.Reset(interval)
	}
}

// FromConfig maps the app config onto retention windows. Failed pending
// signals (14 days) and FAILED trades (30 days) keep the windows the old
// prune.sql used.
func FromConfig(c *config.Config) Config {
	return Config{
		Candle1mDays:       c.Candle1mRetentionDays,
		CandleIntradayDays: c.CandleIntradayRetentionDays,
		AuditTextDays:      c.AuditTextRetentionDays,
		FailedPendingDays:  14,
		FailedTradeDays:    30,
	}
}
