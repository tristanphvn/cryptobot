package models

import (
	"encoding/json"
	"time"

	"github.com/shopspring/decimal"
)

type TradeStatus string

const (
	TradeStatusOpen      TradeStatus = "OPEN"
	TradeStatusClosed    TradeStatus = "CLOSED"
	TradeStatusCancelled TradeStatus = "CANCELLED"
	TradeStatusFailed    TradeStatus = "FAILED"
)

type TradeSide string

const (
	TradeSideBuy  TradeSide = "BUY"
	TradeSideSell TradeSide = "SELL"
)

// ExitReason is the structured taxonomy of how a position came to be
// closed. Persisted on trades.exit_reason (varchar, nullable on OPEN /
// FAILED rows). Standard, not bandaid: downstream dashboards / metrics
// read this column, NOT the free-form Reasoning string — so a new exit
// path must register a new enum value here, not invent a reasoning
// substring.
//
// CRY-16 introduces AI_SELL, TAKE_PROFIT, STOP_LOSS. CRY-22 adds
// TRAILING_STOP. MANUAL_CLOSE and RECONCILE_CLOSE are reserved for the
// operator-action endpoint and the exchange reconciler respectively
// (wiring lives in their own tickets).
//
// Hierarchy when multiple conditions trigger on the same tick (enforced
// in executor/monitor.go):
//   STOP_LOSS > TRAILING_STOP > TAKE_PROFIT > AI_SELL > MANUAL_CLOSE
// Hard exits always take precedence. RECONCILE_CLOSE is out-of-band and
// not part of this hierarchy.
type ExitReason string

const (
	ExitReasonAISell          ExitReason = "AI_SELL"
	ExitReasonTakeProfit      ExitReason = "TAKE_PROFIT"
	ExitReasonStopLoss        ExitReason = "STOP_LOSS"
	ExitReasonTrailingStop    ExitReason = "TRAILING_STOP"
	ExitReasonManualClose     ExitReason = "MANUAL_CLOSE"
	ExitReasonReconcileClose  ExitReason = "RECONCILE_CLOSE"
)

// Trade logs every execution the bot performs.
type Trade struct {
	ID         uint            `gorm:"primaryKey"                              json:"id"`
	Symbol     string          `gorm:"type:varchar(20);not null;index"         json:"symbol"`
	Side       TradeSide       `gorm:"type:varchar(10);not null"               json:"side"`
	Status     TradeStatus     `gorm:"type:varchar(12);not null;default:OPEN"  json:"status"`
	EntryPrice decimal.Decimal `gorm:"type:numeric(20,8);not null"             json:"entry_price"`
	ExitPrice  decimal.Decimal `gorm:"type:numeric(20,8)"                      json:"exit_price"`
	Quantity   decimal.Decimal `gorm:"type:numeric(20,8);not null"             json:"quantity"`

	// CRY-6 cost / fee accounting. EntryCost = EntryPrice * Quantity at the
	// moment of fill (cached so PnL math doesn't need to re-multiply, and so
	// partial-fill reconciliation can record the realized notional cleanly).
	// Fees are settled in FeeCurrency — Binance Spot returns "USDT" when
	// paying in the quote, "BNB" when "Use BNB for fees" is enabled. CRY-9
	// wires real fee data from OrderResponse.Fills; until then executor
	// populates 0 so NetPnL == PnL for new trades and legacy backfill works.
	EntryCost   decimal.Decimal `gorm:"type:numeric(20,8);default:0"            json:"entry_cost"`
	ExitCost    decimal.Decimal `gorm:"type:numeric(20,8);default:0"            json:"exit_cost"`
	EntryFee    decimal.Decimal `gorm:"type:numeric(20,8);default:0"            json:"entry_fee"`
	ExitFee     decimal.Decimal `gorm:"type:numeric(20,8);default:0"            json:"exit_fee"`
	FeeCurrency string          `gorm:"type:varchar(10);default:'USDT'"         json:"fee_currency"`

	PnL           decimal.Decimal `gorm:"column:pnl;type:numeric(20,8)"           json:"pnl"`
	PnLPercent    decimal.Decimal `gorm:"column:pnl_percent;type:numeric(10,4)"   json:"pnl_percent"`
	NetPnL        decimal.Decimal `gorm:"column:net_pnl;type:numeric(20,8);default:0"         json:"net_pnl"`
	NetPnLPercent decimal.Decimal `gorm:"column:net_pnl_percent;type:numeric(10,4);default:0" json:"net_pnl_percent"`

	// ExitReason is the structured taxonomy of how the trade closed
	// (CRY-16). Empty on OPEN / FAILED rows; populated on CLOSED rows
	// by the single closePosition entry point. See models.ExitReason
	// for the value set. Indexed because dashboards filter by it.
	ExitReason ExitReason `gorm:"type:varchar(20);index" json:"exit_reason,omitempty"`

	// IsPaper distinguishes paper-mode simulated trades from live-mode
	// real fills. Set at row creation by the executor / reconciler based
	// on the runtime Mode. The dashboard and stats handlers filter on
	// this to avoid mixing simulated wins/losses into a live KPI surface
	// (and vice versa) after a mode flip. Default false (= live) at the
	// schema level; a one-time migration backfills legacy rows whose
	// `client_order_id` is empty to true, since live-mode trades always
	// carry the CRY-15 idempotency key 'cry5-<pending_signal_id>'.
	IsPaper bool `gorm:"not null;default:false" json:"is_paper"`

	AIConfidence float64 `gorm:"type:numeric(5,4)" json:"ai_confidence"`
	AIReasoning  string  `gorm:"type:text"         json:"ai_reasoning"`

	// Error holds the detailed failure reason when Status=FAILED (slippage_exceeded,
	// binance:<reason>, poll_timeout, etc.). Empty for OPEN / CLOSED rows. Added in
	// CRY-15 — the audit trail needs to distinguish slippage rejections from exchange
	// rejections without forcing the operator to grep logs.
	Error string `gorm:"type:text" json:"error,omitempty"`
	// ExchangeOrderID is Binance's numeric orderId (per /api/v3/order). Indexed so the
	// reconciler can correlate a Binance-side support ticket back to a local row.
	ExchangeOrderID int64 `gorm:"index" json:"exchange_order_id,omitempty"`
	// ClientOrderID is the deterministic "cry5-<pending_signal_id>" idempotency key
	// we submitted to Binance. Persisted so the reconciler can re-lookup by it.
	ClientOrderID string `gorm:"type:varchar(64);index" json:"client_order_id,omitempty"`
	// ExchangeFillData stores the raw Fills[] array from Binance OrderResponse
	// as JSONB. Useful for partial-fill forensics and cross-venue audit.
	// CRY-9 will populate from `OrderResponse.Fills`; nil/empty for legacy rows.
	ExchangeFillData json.RawMessage `gorm:"type:jsonb" json:"exchange_fill_data,omitempty"`

	EntryAt   time.Time  `gorm:"not null" json:"entry_at"`
	ExitAt    *time.Time `json:"exit_at"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

func (Trade) TableName() string { return "trades" }
