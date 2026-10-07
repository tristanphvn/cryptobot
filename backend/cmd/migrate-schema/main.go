// Command migrate-schema connects to the configured Postgres, runs
// capability detection (timescaledb present? → hypertable path; absent
// → plain-table path), and applies the GORM AutoMigrate schema and
// idempotent legacy fixups in database.Migrate.
//
// This is the recommended path for one-shot schema work against a
// managed Postgres (Neon, RDS) where you do NOT want to spin up the
// full backend (collector, executor, persister, hub) just to land DDL.
//
// Standard, not bandaid: separating schema migration from the runtime
// process makes the cutover deterministic — schema lands, command
// exits, then the runtime starts on a known-good schema.
//
// Usage:
//
//	go run ./cmd/migrate-schema
//
// Honours every standard POSTGRES_* env var, including POSTGRES_SSLMODE
// (set to "require" for Neon).
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
		log.Fatalf("database connect: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		log.Fatalf("database migrate: %v", err)
	}
	log.Println("migrate-schema complete")
}
