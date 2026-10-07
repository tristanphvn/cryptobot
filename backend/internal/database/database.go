package database

import (
	"log"
	"time"

	"cryptobot/internal/config"
	"cryptobot/internal/database/models"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// HasTimescale is set during Connect() based on whether the
// `timescaledb` extension is loadable + present after CONNECT. It is
// read by Migrate() to decide between hypertable and plain-table paths
// for the `candles` table. Standard, not bandaid: one capability check
// at startup drives every Timescale-conditional code path so we never
// have a mismatch between "extension installed" and "schema assumes
// hypertable".
//
// NOTE: This is intentionally a package-level var rather than a Config
// field because it reflects runtime DB state, not user intent. Tests
// may set it directly when exercising migration logic against a fake.
var HasTimescale bool

func Connect(cfg *config.Config) (*gorm.DB, error) {
	// PrepareStmt is intentionally NOT enabled. Neon's PgBouncer pools
	// connections in transaction mode, where prepared statements do not
	// survive across check-outs — turning PrepareStmt on would silently
	// break the runtime on managed Postgres. If you ever want server-side
	// prepares for performance, connect through the direct (non-pooled)
	// Neon endpoint instead, or move to a session-mode pooler.
	db, err := gorm.Open(postgres.Open(cfg.DSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	})
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(cfg.DBMaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.DBMaxIdleConns)
	if cfg.DBConnMaxLifetimeMin > 0 {
		sqlDB.SetConnMaxLifetime(time.Duration(cfg.DBConnMaxLifetimeMin) * time.Minute)
	}
	if cfg.DBConnMaxIdleTimeMin > 0 {
		sqlDB.SetConnMaxIdleTime(time.Duration(cfg.DBConnMaxIdleTimeMin) * time.Minute)
	}
	log.Printf("database: connected dsn=%s pool=open=%d/idle=%d lifetime=%dm idle_timeout=%dm",
		cfg.DSNRedacted(), cfg.DBMaxOpenConns, cfg.DBMaxIdleConns,
		cfg.DBConnMaxLifetimeMin, cfg.DBConnMaxIdleTimeMin)

	// Capability detection — does this DB ALREADY have TimescaleDB
	// installed? We deliberately do NOT run CREATE EXTENSION here: we
	// detect what's present, never install. Operators decide whether a
	// given DB runs the hypertable path by installing (or not installing)
	// the extension at provisioning time:
	//
	//   - Local docker image (timescale/timescaledb:*-pg15) auto-installs
	//     the extension during initdb → detection returns present → hypertable.
	//   - Managed Postgres (Neon, RDS) without an explicit CREATE EXTENSION
	//     → detection returns absent → plain-table path. The persister's
	//     ON CONFLICT (symbol, interval, open_time) clause still works
	//     because that unique index is declared by GORM tags on the
	//     model and exists on both paths.
	//
	// Standard, not bandaid: "detect, don't install" means a manual
	// decision to enable Timescale on a managed DB is required and
	// auditable, never silently activated by the runtime.
	var n int
	row := db.Raw("SELECT count(*) FROM pg_extension WHERE extname = 'timescaledb'").Row()
	if err := row.Scan(&n); err != nil {
		log.Printf("database: timescaledb capability probe failed: %v", err)
	}
	HasTimescale = n > 0
	if HasTimescale {
		log.Println("database: timescaledb=present — candles will use hypertable path")
	} else {
		log.Println("database: timescaledb=absent — candles will use plain-table path")
	}

	return db, nil
}

// Migrate auto-migrates all GORM models.
func Migrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
		&models.Candle{},
		&models.Trade{},
		&models.Position{},
		&models.AuditLog{},
		&models.PendingSignal{},
		&models.BacktestAICache{},
	); err != nil {
		return err
	}

	// CRY-6 follow-up: drop the speculative event_logs table. It was
	// added intending to be a generic state-change stream, but every
	// event we currently produce already has a natural home —
	// audit_logs (AI inference), pending_signals (approval state
	// machine), trades (order lifecycle). Keeping an empty table
	// around is metadata noise and invites future confusion. Revisit
	// only when there is a concrete event with no other home.
	db.Exec("DROP TABLE IF EXISTS event_logs")

	// Fix the legacy GORM PnL → pn_l naming bug. The original Migrate()
	// did `ALTER TABLE … RENAME` but those fail silently on a DB where
	// AutoMigrate has already created the correct column from the
	// `gorm:"column:pnl"` tag — leaving duplicate columns
	// (`pn_l` AND `pnl`) lingering forever. The block below handles
	// both the fresh-broken case (rename) and the duplicate-state case
	// (copy data + drop broken). Idempotent: a no-op once cleaned.
	db.Exec(`
		DO $$
		BEGIN
			-- trades.pnl
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='trades' AND column_name='pn_l') THEN
				IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='trades' AND column_name='pnl') THEN
					UPDATE trades SET pnl = pn_l WHERE pnl IS NULL AND pn_l IS NOT NULL;
					ALTER TABLE trades DROP COLUMN pn_l;
				ELSE
					ALTER TABLE trades RENAME COLUMN pn_l TO pnl;
				END IF;
			END IF;
			-- trades.pnl_percent
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='trades' AND column_name='pn_l_percent') THEN
				IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='trades' AND column_name='pnl_percent') THEN
					UPDATE trades SET pnl_percent = pn_l_percent WHERE pnl_percent IS NULL AND pn_l_percent IS NOT NULL;
					ALTER TABLE trades DROP COLUMN pn_l_percent;
				ELSE
					ALTER TABLE trades RENAME COLUMN pn_l_percent TO pnl_percent;
				END IF;
			END IF;
			-- positions.unrealized_pnl (CRY-6: pre-existing UnrealizedPnL inherited the bug)
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='positions' AND column_name='unrealized_pn_l') THEN
				IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='positions' AND column_name='unrealized_pnl') THEN
					UPDATE positions SET unrealized_pnl = unrealized_pn_l WHERE unrealized_pnl IS NULL AND unrealized_pn_l IS NOT NULL;
					ALTER TABLE positions DROP COLUMN unrealized_pn_l;
				ELSE
					ALTER TABLE positions RENAME COLUMN unrealized_pn_l TO unrealized_pnl;
				END IF;
			END IF;
			-- positions.realized_pnl (CRY-6 new field that hit the same bug)
			IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='positions' AND column_name='realized_pn_l') THEN
				IF EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='positions' AND column_name='realized_pnl') THEN
					UPDATE positions SET realized_pnl = realized_pn_l WHERE (realized_pnl IS NULL OR realized_pnl = 0) AND realized_pn_l IS NOT NULL;
					ALTER TABLE positions DROP COLUMN realized_pn_l;
				ELSE
					ALTER TABLE positions RENAME COLUMN realized_pn_l TO realized_pnl;
				END IF;
			END IF;
		END $$;
	`)

	// Candles partitioning — capability-gated.
	//
	// Timescale path: drop GORM's autoincrement-PK and rebuild it as
	// (id, open_time) so it includes the partition column (a hypertable
	// hard requirement), then convert the table to a hypertable.
	//
	// Plain-Postgres path (Neon, vanilla PG, RDS without Timescale):
	// keep the GORM-generated `id` PK as-is. The persister's UPSERT
	// targets the unique index on (symbol, interval, open_time) declared
	// in models.Candle's struct tags — that index exists on both paths
	// so ON CONFLICT semantics are identical.
	if HasTimescale {
		log.Println("database: candles → hypertable path")
		db.Exec("ALTER TABLE candles DROP CONSTRAINT IF EXISTS candles_pkey")
		db.Exec("ALTER TABLE candles ADD CONSTRAINT candles_pkey PRIMARY KEY (id, open_time)")
		db.Exec("SELECT create_hypertable('candles', 'open_time', if_not_exists => TRUE, migrate_data => TRUE)")
		// CRY-38: candles were 82% of the database with nothing bounding
		// them. Compress chunks older than 7 days, segmented by (symbol,
		// interval) so a batch never mixes intervals — that lets the
		// retention job count / prune one interval from batch metadata.
		// id rides in orderby because it is part of the primary key.
		// Re-running on an already-compressed table only emits a NOTICE.
		if err := db.Exec(`ALTER TABLE candles SET (timescaledb.compress,
			timescaledb.compress_segmentby = 'symbol, interval',
			timescaledb.compress_orderby = 'open_time DESC, id')`).Error; err != nil {
			log.Printf("database: candles compression settings failed: %v", err)
		} else if err := db.Exec("SELECT add_compression_policy('candles', INTERVAL '7 days', if_not_exists => TRUE)").Error; err != nil {
			log.Printf("database: candles compression policy failed: %v", err)
		}
	} else {
		log.Println("database: candles → plain-table path (no timescaledb)")
	}

	// CRY-16: replace the legacy unconditional uniqueIndex on
	// positions.symbol with a PARTIAL unique index `WHERE is_open = true`.
	// The lifecycle has always been OPEN → CLOSED, and a new OPEN can
	// follow on the same symbol — the legacy index blocked the second
	// row insert and silently froze the bot at one position per symbol
	// for the lifetime of the database. Idempotent on both legs: the
	// DROP no-ops if the legacy index was never there, the CREATE
	// IF NOT EXISTS no-ops if the partial index already exists.
	db.Exec(`DO $$
		BEGIN
			-- GORM's auto-generated name for the legacy uniqueIndex on
			-- positions.symbol; drop unconditionally so subsequent
			-- INSERTs on the same symbol succeed once the original
			-- position has been CLOSED.
			IF EXISTS (
				SELECT 1 FROM pg_indexes
				 WHERE schemaname = current_schema()
				   AND tablename = 'positions'
				   AND indexname = 'idx_positions_symbol'
			) THEN
				DROP INDEX idx_positions_symbol;
			END IF;
			IF EXISTS (
				SELECT 1 FROM pg_indexes
				 WHERE schemaname = current_schema()
				   AND tablename = 'positions'
				   AND indexname = 'uni_positions_symbol'
			) THEN
				DROP INDEX uni_positions_symbol;
			END IF;
		END $$;`)
	db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS positions_unique_open_symbol
	             ON positions (symbol) WHERE is_open = true`)

	// trades.is_paper backfill — idempotent + semantically correct for both
	// legacy migration and any future under-set rows. Live-mode trades
	// always carry the CRY-15 idempotency key (`client_order_id` like
	// 'cry5-%'); paper-mode trades never do. We can therefore distinguish
	// them at the schema level without an explicit migration timestamp.
	// Re-running this UPDATE is a no-op once rows are correctly flagged.
	db.Exec(`
		UPDATE trades
		   SET is_paper = TRUE
		 WHERE is_paper = FALSE
		   AND (client_order_id IS NULL OR client_order_id = '')
	`)

	return nil
}
