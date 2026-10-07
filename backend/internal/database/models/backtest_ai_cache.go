package models

import "time"

// BacktestAICache memoises an LLM analysis at a (symbol, candle-open-time,
// model) triple so re-running the backtest against the same date range is
// cheap. CRY-10 spec calls for ≥ 95% hit rate on the second run.
//
// The cache key uses candle open time (not now()) because the analyzer is
// deterministic given the same window — different runs of cmd/backtest with
// the same --from/--to feed identical MarketContext to the LLM. Reusing the
// stored response is faithful re-execution, not staleness.
//
// We store the parsed signal fields directly so the cache lookup path skips
// JSON parsing entirely. RawResponse is kept for audit / re-parsing if the
// signal schema later grows fields. ModelName is the cache-bust knob — swap
// models and every row mismatches, forcing a fresh inference.
type BacktestAICache struct {
	ID             uint      `gorm:"primaryKey" json:"id"`
	Symbol         string    `gorm:"type:varchar(20);not null;index:idx_backtest_ai_cache_lookup,unique,priority:1" json:"symbol"`
	CandleOpenTime time.Time `gorm:"not null;index:idx_backtest_ai_cache_lookup,unique,priority:2" json:"candle_open_time"`
	Model          string    `gorm:"type:varchar(64);not null;index:idx_backtest_ai_cache_lookup,unique,priority:3" json:"model"`

	Action     string  `gorm:"type:varchar(8);not null" json:"action"` // BUY | SELL | HOLD
	Confidence float64 `gorm:"type:numeric(6,4);not null" json:"confidence"`
	Reasoning  string  `gorm:"type:text"                  json:"reasoning"`
	StopLoss   float64 `gorm:"type:numeric(20,8);default:0" json:"stop_loss"`
	TakeProfit float64 `gorm:"type:numeric(20,8);default:0" json:"take_profit"`

	RawResponse string    `gorm:"type:text" json:"raw_response,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func (BacktestAICache) TableName() string { return "backtest_ai_cache" }
