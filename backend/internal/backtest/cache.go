package backtest

import (
	"log"
	"time"

	"cryptobot/internal/ai"
	"cryptobot/internal/database/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DBCache is the persistent AICache used by cmd/backtest. Re-running the
// CLI against the same (--symbols, --from, --to, model) hits this table
// instead of the LLM, satisfying the CRY-10 "≥ 95% hit on warm run"
// acceptance criterion.
//
// We intentionally do NOT cache through the HTTP /api/v1/backtest handler;
// it constructs an Engine with NoopCache so ad-hoc API runs cannot bloat
// the cache table with throwaway entries. Only the CLI opts in.
type DBCache struct {
	db *gorm.DB
}

func NewDBCache(db *gorm.DB) *DBCache {
	return &DBCache{db: db}
}

// Get returns a cached analysis, populating only the fields the engine
// actually reads (Signal). Audit fields (Prompt / RawResponse / timing)
// are NOT reconstructed — they live on the cache row for offline forensics
// but the live re-run doesn't depend on them.
//
// Find + Limit(1) instead of First because First returns ErrRecordNotFound
// on a miss, which GORM's default logger prints as a warning per query.
// Backtest cold runs miss every row (~8760 for a year of 1h candles) —
// First would flood the console with rows:0 traces. Find + RowsAffected
// is the silent miss path.
func (c *DBCache) Get(symbol string, candleOpenTime time.Time, model string) (*ai.AnalysisResult, bool) {
	var row models.BacktestAICache
	res := c.db.Where("symbol = ? AND candle_open_time = ? AND model = ?", symbol, candleOpenTime.UTC(), model).
		Limit(1).
		Find(&row)
	if res.Error != nil {
		log.Printf("[backtest-cache] read error: %v", res.Error)
		return nil, false
	}
	if res.RowsAffected == 0 {
		return nil, false
	}
	return &ai.AnalysisResult{
		Signal: &ai.Signal{
			Action:     row.Action,
			Confidence: row.Confidence,
			Reasoning:  row.Reasoning,
			StopLoss:   row.StopLoss,
			TakeProfit: row.TakeProfit,
		},
		RawResponse: row.RawResponse,
		ReceivedAt:  row.CreatedAt,
	}, true
}

// Put inserts a row; on conflict the older row wins (signal is determ-
// inistic so this is a no-op race, not a correctness issue). Errors are
// logged and swallowed — a cache write failing must NOT abort the
// backtest run.
//
// clause.OnConflict{DoNothing: true} matches the (symbol, candle_open_time,
// model) unique index. Cleaner than FirstOrCreate, which would do a
// SELECT-then-INSERT round trip and trigger the same ErrRecordNotFound
// warning Get used to.
func (c *DBCache) Put(symbol string, candleOpenTime time.Time, model string, result *ai.AnalysisResult) {
	if result == nil || result.Signal == nil {
		return
	}
	row := models.BacktestAICache{
		Symbol:         symbol,
		CandleOpenTime: candleOpenTime.UTC(),
		Model:          model,
		Action:         result.Signal.Action,
		Confidence:     result.Signal.Confidence,
		Reasoning:      result.Signal.Reasoning,
		StopLoss:       result.Signal.StopLoss,
		TakeProfit:     result.Signal.TakeProfit,
		RawResponse:    result.RawResponse,
	}
	if err := c.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
		log.Printf("[backtest-cache] write error: %v", err)
	}
}
