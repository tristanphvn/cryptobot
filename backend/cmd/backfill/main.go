// Command backfill seeds the candles hypertable with historical klines from
// Binance for the CRY-10 backtest sweep and any other offline analysis that
// needs a year-deep history table.
//
// Standard, not bandaid: idempotent via the (symbol, interval, open_time)
// unique index — re-running converges to the latest exchange snapshot
// rather than appending duplicates. Rate-limit safe at 200 ms between
// /api/v3/klines calls (5 req/s, well inside the 6000 weight/min budget).
//
// Usage:
//
//	go run ./cmd/backfill --symbols=BTCUSDT,ETHUSDT --months=12 --interval=1h
//
// Output is per-symbol row counts; non-zero exit code on any fetch error
// (DB upsert errors fail-fast on the first symbol).
package main

import (
	"flag"
	"log"
	"strings"
	"time"

	"cryptobot/internal/config"
	"cryptobot/internal/database"
	"cryptobot/internal/exchange"
)

func main() {
	var (
		symbolsFlag = flag.String("symbols", "BTCUSDT", "comma-separated symbols to backfill (e.g. BTCUSDT,ETHUSDT)")
		months      = flag.Int("months", 12, "how many calendar months back from now")
		interval    = flag.String("interval", "1h", "kline interval (1m/5m/15m/1h/4h/1d/...)")
	)
	flag.Parse()

	symbols := splitSymbols(*symbolsFlag)
	if len(symbols) == 0 {
		log.Fatalf("--symbols is required (got empty after parsing %q)", *symbolsFlag)
	}
	if *months <= 0 {
		log.Fatalf("--months must be positive, got %d", *months)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database: %v", err)
	}

	end := time.Now().UTC()
	start := end.AddDate(0, -*months, 0)
	log.Printf("backfill: %s → %s (%s candles) for %v",
		start.Format(time.RFC3339), end.Format(time.RFC3339), *interval, symbols)

	for _, sym := range symbols {
		t0 := time.Now()
		candles, err := exchange.FetchHistoricalCandlesRange(cfg.BinanceRESTURL, sym, *interval, start, end)
		if err != nil {
			log.Fatalf("fetch %s: %v", sym, err)
		}
		rows, err := exchange.UpsertCandles(db, candles)
		if err != nil {
			log.Fatalf("upsert %s: %v", sym, err)
		}
		log.Printf("  %s: fetched=%d upserted=%d in %s",
			sym, len(candles), rows, time.Since(t0).Round(time.Millisecond))
	}
	log.Println("backfill complete")
}

// splitSymbols normalises the --symbols flag: trims whitespace, drops empty
// entries (so "BTCUSDT, , ETHUSDT" still parses cleanly), uppercases each.
func splitSymbols(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		out = append(out, strings.ToUpper(s))
	}
	return out
}
