// Command prune runs the CRY-38 data-lifecycle rules once — the same code
// the server runs on its RETENTION_INTERVAL schedule. It replaces the
// manual backend/scripts/prune.sql.
//
//	go run ./cmd/prune                        # apply retention once
//	go run ./cmd/prune -clear-backtest-cache  # also drop the backtest AI cache
//
// The backtest AI cache is never cleared automatically: it memoises
// Ollama inference that is slow to regenerate.
package main

import (
	"context"
	"flag"
	"log"
	"time"

	"cryptobot/internal/config"
	"cryptobot/internal/database"
	"cryptobot/internal/retention"
)

func main() {
	clearCache := flag.Bool("clear-backtest-cache", false, "also delete backtest_ai_cache rows older than 1 hour")
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}

	start := time.Now()
	rep, err := retention.Run(context.Background(), db, retention.FromConfig(cfg), database.HasTimescale, start)
	if err != nil {
		log.Fatalf("retention: %v (%s)", err, rep)
	}
	log.Printf("retention ok in %s: %s", time.Since(start).Round(time.Millisecond), rep)

	if *clearCache {
		// 1-hour grace so a backtest running right now keeps its fresh rows.
		res := db.Exec("DELETE FROM backtest_ai_cache WHERE created_at < ?", start.Add(-time.Hour))
		if res.Error != nil {
			log.Fatalf("backtest cache: %v", res.Error)
		}
		log.Printf("backtest_ai_cache: deleted %d rows", res.RowsAffected)
	}
}
