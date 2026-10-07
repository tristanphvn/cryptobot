package models

import (
	"time"

	"github.com/shopspring/decimal"
)

// AuditLog stores the full evidence trail for every AI evaluation:
// the exact prompt sent, the raw LLM response, the parsed signal,
// and the market context at the time of analysis.
type AuditLog struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// Link to the trade that resulted from this signal (nullable — HOLD signals don't produce trades)
	TradeID *uint `gorm:"index" json:"trade_id,omitempty"`

	// What we asked
	Symbol       string          `gorm:"type:varchar(20);not null;index" json:"symbol"`
	Interval     string          `gorm:"type:varchar(5);not null"        json:"interval"`
	Prompt       string          `gorm:"type:text;not null"              json:"prompt"`
	MarketPrice  decimal.Decimal `gorm:"type:numeric(20,8)"              json:"market_price"`
	MarketRSI    float64         `gorm:"type:numeric(10,4)"              json:"market_rsi"`
	MarketMACD   float64         `gorm:"type:numeric(20,8)"              json:"market_macd"`
	MarketSignal float64         `gorm:"type:numeric(20,8)"              json:"market_signal_line"`
	MarketVolume float64         `gorm:"type:numeric(20,8)"              json:"market_volume"`

	// What the LLM returned
	RawResponse string `gorm:"type:text;not null" json:"raw_response"`

	// Parsed result
	Action     string  `gorm:"type:varchar(10);not null" json:"action"`
	Confidence float64 `gorm:"type:numeric(5,4)"         json:"confidence"`
	Reasoning  string  `gorm:"type:text"                 json:"reasoning"`

	// Whether the signal was acted upon
	Executed bool   `gorm:"default:false" json:"executed"`
	BlockedBy string `gorm:"type:varchar(50)" json:"blocked_by,omitempty"` // e.g. "risk:daily_drawdown", "confidence", ""

	// Timing
	PromptSentAt    time.Time `gorm:"not null" json:"prompt_sent_at"`
	ResponseAt      time.Time `gorm:"not null" json:"response_at"`
	InferenceTimeMs int64     `json:"inference_time_ms"`

	// CRY-22 position context — populated when the AI tick had an open
	// position on this symbol (so the LLM saw the PositionContext block
	// in the prompt). Nullable: a fresh BUY-only evaluation, a HOLD on
	// a symbol with no position, or any monitor-synthesized row leaves
	// all five NULL.
	//
	// Forensic query intent — answers questions like:
	//   "How often did the LLM HOLD on a winner that subsequently lost
	//    the gain?"
	//
	//   SELECT al.symbol, al.confidence, al.position_peak_price,
	//          al.position_drawdown_from_peak_pct, al.reasoning
	//   FROM audit_logs al
	//   WHERE al.action = 'HOLD'
	//     AND al.position_peak_price IS NOT NULL
	//     AND al.position_drawdown_from_peak_pct > 2.0
	//   ORDER BY al.created_at DESC;
	//
	// Pointer-of-primitive (*float64) preserves NULL vs 0.0 distinction
	// in SQL — matches the GORM convention already used for
	// Position.StopLoss / TakeProfit. Explicit `column:` tag on the
	// PnL field to dodge the GORM `PnL → pn_l` naming bug.
	PositionEntryPrice          *decimal.Decimal `gorm:"type:numeric(20,8)"                                         json:"position_entry_price,omitempty"`
	PositionUnrealizedPnLPct    *float64         `gorm:"column:position_unrealized_pnl_pct;type:numeric(10,4)"      json:"position_unrealized_pnl_pct,omitempty"`
	PositionPeakPrice           *decimal.Decimal `gorm:"type:numeric(20,8)"                                         json:"position_peak_price,omitempty"`
	PositionDrawdownFromPeakPct *float64         `gorm:"type:numeric(10,4)"                                         json:"position_drawdown_from_peak_pct,omitempty"`
	PositionHoursHeld           *float64         `gorm:"type:numeric(10,4)"                                         json:"position_hours_held,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

func (AuditLog) TableName() string { return "audit_logs" }
