// Command migrate-backfill seeds the CRY-6 cost/PnL columns on existing
// trades and positions.
//
// Standard, not bandaid: every UPDATE has a WHERE clause that skips
// rows already backfilled, so the command is safe to run repeatedly
// (idempotent). The columns themselves are added by GORM AutoMigrate
// on server start with NULL/0 defaults — this command only enriches
// legacy rows where the executor wrote nothing.
//
// Fee data cannot be reconstructed from history (Binance fee receipts
// are not persisted before CRY-9), so legacy EntryFee / ExitFee stay 0
// and NetPnL == PnL for legacy CLOSED trades. New trades from CRY-9
// onwards will carry real fee figures and a different NetPnL.
//
// Usage:
//
//	go run ./cmd/migrate-backfill
package main

import (
	"log"

	"cryptobot/internal/config"
	"cryptobot/internal/database"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database: %v", err)
	}

	// Trade.EntryCost = EntryPrice * Quantity for any row where the
	// executor never wrote it (legacy rows pre-CRY-6 always show 0).
	res := db.Exec(`
		UPDATE trades
		   SET entry_cost = entry_price * quantity
		 WHERE (entry_cost IS NULL OR entry_cost = 0)
		   AND entry_price > 0
		   AND quantity > 0
	`)
	if res.Error != nil {
		log.Fatalf("backfill entry_cost: %v", res.Error)
	}
	log.Printf("trades.entry_cost backfilled: %d rows", res.RowsAffected)

	// Same for ExitCost on CLOSED trades.
	res = db.Exec(`
		UPDATE trades
		   SET exit_cost = exit_price * quantity
		 WHERE status = 'CLOSED'
		   AND (exit_cost IS NULL OR exit_cost = 0)
		   AND exit_price > 0
		   AND quantity > 0
	`)
	if res.Error != nil {
		log.Fatalf("backfill exit_cost: %v", res.Error)
	}
	log.Printf("trades.exit_cost backfilled: %d rows", res.RowsAffected)

	// For legacy CLOSED trades, NetPnL = PnL because we have no fee
	// data. Only touch rows where net_pnl is still the default 0 AND
	// the gross PnL is non-zero (otherwise the row is genuinely flat,
	// not stale).
	res = db.Exec(`
		UPDATE trades
		   SET net_pnl = pnl,
		       net_pnl_percent = pnl_percent
		 WHERE status = 'CLOSED'
		   AND (net_pnl IS NULL OR net_pnl = 0)
		   AND pnl IS NOT NULL
		   AND pnl <> 0
	`)
	if res.Error != nil {
		log.Fatalf("backfill net_pnl: %v", res.Error)
	}
	log.Printf("trades.net_pnl backfilled: %d rows", res.RowsAffected)

	// Aggregate per-symbol from the now-backfilled trades into the
	// Position row. Idempotent: WHERE realized_pnl = 0 skips already-
	// updated positions. NetPnL is the source of truth (= PnL for
	// legacy rows where fee unknown).
	res = db.Exec(`
		UPDATE positions p
		   SET realized_pnl = sub.sum_pnl,
		       total_fees   = sub.sum_fees,
		       total_cost   = sub.sum_cost
		  FROM (
		       SELECT symbol,
		              COALESCE(SUM(net_pnl), 0)                          AS sum_pnl,
		              COALESCE(SUM(entry_fee + exit_fee), 0)             AS sum_fees,
		              COALESCE(SUM(entry_cost), 0)                       AS sum_cost
		         FROM trades
		        WHERE status = 'CLOSED'
		        GROUP BY symbol
		  ) AS sub
		 WHERE p.symbol = sub.symbol
		   AND (p.realized_pnl IS NULL OR p.realized_pnl = 0)
	`)
	if res.Error != nil {
		log.Fatalf("backfill position aggregates: %v", res.Error)
	}
	log.Printf("positions aggregates backfilled: %d rows", res.RowsAffected)

	log.Println("migrate-backfill complete")
}
