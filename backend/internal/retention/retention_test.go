package retention

import (
	"context"
	"os"
	"testing"
	"time"

	"cryptobot/internal/database/models"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func ago(days int) time.Time { return now.Add(-time.Duration(days) * 24 * time.Hour) }

func defaultCfg() Config {
	return Config{Candle1mDays: 30, CandleIntradayDays: 90, AuditTextDays: 30, FailedPendingDays: 14, FailedTradeDays: 30}
}

func seedCandle(t *testing.T, db *gorm.DB, interval string, openTime time.Time) {
	t.Helper()
	px := decimal.NewFromInt(100)
	if err := db.Create(&models.Candle{Symbol: "BTCUSDT", Interval: interval, OpenTime: openTime, CloseTime: openTime.Add(time.Minute),
		Open: px, High: px, Low: px, Close: px, Volume: px}).Error; err != nil {
		t.Fatalf("seed candle: %v", err)
	}
}

func countCandles(db *gorm.DB, interval string) int64 {
	var n int64
	db.Model(&models.Candle{}).Where("interval = ?", interval).Count(&n)
	return n
}

func TestRun_PlainPostgresPath(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(&models.Candle{}, &models.AuditLog{}, &models.PendingSignal{}, &models.Trade{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, c := range []struct {
		iv   string
		days int
	}{{"1m", 31}, {"1m", 29}, {"5m", 91}, {"15m", 91}, {"5m", 89}, {"1h", 400}, {"4h", 400}, {"1d", 900}} {
		seedCandle(t, db, c.iv, ago(c.days))
	}
	oldAudit := models.AuditLog{Symbol: "BTCUSDT", Interval: "1h", Prompt: "big prompt", RawResponse: "{...}", Action: "HOLD",
		Reasoning: "kept", PromptSentAt: ago(31), ResponseAt: ago(31)}
	newAudit := models.AuditLog{Symbol: "BTCUSDT", Interval: "1h", Prompt: "fresh", RawResponse: "{...}", Action: "BUY",
		PromptSentAt: ago(1), ResponseAt: ago(1)}
	db.Create(&oldAudit)
	db.Create(&newAudit)
	for i, st := range []models.PendingSignalStatus{"expired", "submitted", "pending"} {
		db.Create(&models.PendingSignal{Symbol: "BTCUSDT", Interval: "1h", AuditLogID: uint(100 + i), Action: "BUY", Confidence: 0.9,
			MarketPrice: decimal.NewFromInt(1), Status: st, ExpiresAt: ago(20)})
	}
	db.Create(&models.Trade{Symbol: "BTCUSDT", Side: models.TradeSideBuy, Status: "FAILED", EntryPrice: decimal.NewFromInt(1),
		Quantity: decimal.NewFromInt(1), EntryAt: ago(40), CreatedAt: ago(40)})
	db.Create(&models.Trade{Symbol: "BTCUSDT", Side: models.TradeSideBuy, Status: models.TradeStatusClosed, EntryPrice: decimal.NewFromInt(1),
		Quantity: decimal.NewFromInt(1), EntryAt: ago(40), CreatedAt: ago(40)})

	rep, err := Run(context.Background(), db, defaultCfg(), false, now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Candles1m != 1 || rep.CandlesIntraday != 2 || rep.AuditTextRows != 1 || rep.PendingSignals != 1 || rep.FailedTrades != 1 {
		t.Fatalf("report=%s", rep)
	}
	for iv, want := range map[string]int64{"1m": 1, "5m": 1, "15m": 0, "1h": 1, "4h": 1, "1d": 1} {
		if got := countCandles(db, iv); got != want {
			t.Errorf("%s candles=%d want %d", iv, got, want)
		}
	}
	var a models.AuditLog
	db.First(&a, oldAudit.ID)
	if a.Prompt != "" || a.RawResponse != "" || a.Reasoning != "kept" || a.Action != "HOLD" {
		t.Fatalf("old audit row must keep structured columns and lose text: %+v", a)
	}
	var fresh models.AuditLog
	db.First(&fresh, newAudit.ID)
	if fresh.Prompt != "fresh" {
		t.Fatal("fresh audit text must be kept")
	}
	var pending, trades int64
	db.Model(&models.PendingSignal{}).Count(&pending)
	db.Model(&models.Trade{}).Count(&trades)
	if pending != 2 || trades != 1 {
		t.Fatalf("pending=%d trades=%d want 2 / 1 (submitted + pending kept, CLOSED trade kept)", pending, trades)
	}

	// Idempotent: a second run removes nothing.
	if rep, _ := Run(context.Background(), db, defaultCfg(), false, now); rep != (Report{}) {
		t.Fatalf("second run=%s want all zero", rep)
	}
	// 0 disables a rule.
	seedCandle(t, db, "1m", ago(60))
	if rep, _ := Run(context.Background(), db, Config{}, false, now); rep.Candles1m != 0 || countCandles(db, "1m") != 2 {
		t.Fatal("disabled rule must not delete")
	}
}

// TestRun_TimescaleCompressedChunks needs a throwaway TimescaleDB, e.g.:
//
//	RETENTION_TIMESCALE_DSN="host=localhost port=55432 user=postgres password=x dbname=rt sslmode=disable" \
//	  go test ./internal/retention/
//
// It DROPS and recreates the candles table — never point it at real data.
// The schema mirrors the statements database.Migrate runs.
func TestRun_TimescaleCompressedChunks(t *testing.T) {
	dsn := os.Getenv("RETENTION_TIMESCALE_DSN")
	if dsn == "" {
		t.Skip("RETENTION_TIMESCALE_DSN not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.Exec("DROP TABLE IF EXISTS candles CASCADE")
	if err := db.AutoMigrate(&models.Candle{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{
		"ALTER TABLE candles DROP CONSTRAINT IF EXISTS candles_pkey",
		"ALTER TABLE candles ADD CONSTRAINT candles_pkey PRIMARY KEY (id, open_time)",
		"SELECT create_hypertable('candles', 'open_time', if_not_exists => TRUE, migrate_data => TRUE)",
		`ALTER TABLE candles SET (timescaledb.compress, timescaledb.compress_segmentby = 'symbol, interval', timescaledb.compress_orderby = 'open_time DESC, id')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// 150 days of hourly rows under every interval label, so each chunk
	// mixes intervals the way the real table does.
	end := time.Now().UTC().Truncate(time.Hour)
	if err := db.Exec(`INSERT INTO candles(symbol, interval, open_time, close_time, open, high, low, close, volume)
		SELECT 'BTCUSDT', iv, t, t + interval '59 minutes', 1, 1, 1, 1, 1
		FROM unnest(ARRAY['1m','5m','15m','1h','4h','1d']) iv,
		     generate_series(?::timestamptz - interval '150 days', ?::timestamptz, interval '1 hour') t`, end, end).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := db.Exec("SELECT compress_chunk(c, if_not_compressed => TRUE) FROM show_chunks('candles', older_than => INTERVAL '7 days') c").Error; err != nil {
		t.Fatalf("compress: %v", err)
	}
	rows := map[string]int64{}
	for _, iv := range []string{"1h", "4h", "1d"} {
		rows[iv] = countCandles(db, iv)
	}

	cfg := Config{Candle1mDays: 30, CandleIntradayDays: 90}
	rep, err := Run(context.Background(), db, cfg, true, time.Now())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if rep.Candles1m == 0 || rep.CandlesIntraday == 0 {
		t.Fatalf("expected deletions, report=%s", rep)
	}
	for iv, want := range rows {
		if got := countCandles(db, iv); got != want {
			t.Errorf("%s candles=%d want %d (must not be pruned)", iv, got, want)
		}
	}
	// Nothing older than window + one chunk interval survives; nothing inside the window is lost.
	for iv, window := range map[string]int{"1m": 30, "5m": 90, "15m": 90} {
		var oldest time.Time
		db.Raw("SELECT min(open_time) FROM candles WHERE interval = ?", iv).Scan(&oldest)
		age := time.Since(oldest)
		if age < time.Duration(window)*24*time.Hour || age > time.Duration(window+7)*24*time.Hour {
			t.Errorf("oldest %s is %s old, want within %d–%d days", iv, age.Round(time.Hour), window, window+7)
		}
	}
	// Pruned chunks are compressed again, not left decompressed.
	var uncompressedOld int64
	db.Raw(`SELECT count(*) FROM timescaledb_information.chunks
		WHERE hypertable_name = 'candles' AND range_end < now() - interval '7 days' AND NOT is_compressed`).Scan(&uncompressedOld)
	if uncompressedOld != 0 {
		t.Fatalf("%d old chunks left uncompressed after pruning", uncompressedOld)
	}
	// Second run is a no-op.
	if rep, err := Run(context.Background(), db, cfg, true, time.Now()); err != nil || rep.Candles1m != 0 || rep.CandlesIntraday != 0 {
		t.Fatalf("second run=%s err=%v want no deletions", rep, err)
	}
}
