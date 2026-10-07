package models

import (
	"time"

	"github.com/shopspring/decimal"
)

// Position tracks the bot's current holdings and cash balance.
type Position struct {
	ID            uint            `gorm:"primaryKey"                            json:"id"`
	// Symbol carries an UNCONDITIONAL index for lookup speed and a
	// PARTIAL UNIQUE constraint enforced via raw SQL in database.Migrate
	// (positions_unique_open_symbol — `WHERE is_open = true`). The partial
	// shape is mandatory: a position lifecycle is OPEN → CLOSED, and a
	// new OPEN may follow on the same symbol later, so the historical
	// row set on a symbol grows monotonically. A pre-CRY-16 unconditional
	// uniqueIndex prevented that second OPEN — the migration drops the
	// legacy index if present.
	Symbol        string          `gorm:"type:varchar(20);not null;index"      json:"symbol"`
	Side          TradeSide       `gorm:"type:varchar(10);not null"             json:"side"`
	Quantity      decimal.Decimal `gorm:"type:numeric(20,8);not null"           json:"quantity"`
	AvgEntryPrice decimal.Decimal `gorm:"type:numeric(20,8);not null"           json:"avg_entry_price"`
	CurrentPrice decimal.Decimal `gorm:"type:numeric(20,8)"                                            json:"current_price"`
	// Explicit column tag to dodge GORM's PnL → pn_l naming bug — same
	// workaround the Trade model uses for `pnl` / `pnl_percent`. Without
	// the tag GORM creates `unrealized_pn_l`, splitting the audit trail.
	UnrealizedPnL decimal.Decimal `gorm:"column:unrealized_pnl;type:numeric(20,8)"                     json:"unrealized_pnl"`

	// CRY-6 lifetime aggregates per symbol. RealizedPnL sums NetPnL of
	// every CLOSED trade on this symbol (so the portfolio view does not
	// have to GROUP BY at query time). TotalFees and TotalCost likewise
	// accumulate from each leg. Updated by the executor at trade close;
	// backfill cmd seeds from existing closed trades. Explicit column
	// tag on RealizedPnL for the same PnL → pn_l naming-bug workaround.
	RealizedPnL decimal.Decimal `gorm:"column:realized_pnl;type:numeric(20,8);default:0"             json:"realized_pnl"`
	TotalFees   decimal.Decimal `gorm:"type:numeric(20,8);default:0"                                 json:"total_fees"`
	TotalCost   decimal.Decimal `gorm:"type:numeric(20,8);default:0"                                 json:"total_cost"`

	IsOpen    bool      `gorm:"default:true" json:"is_open"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// CRY-16 immutable risk snapshot. Captured from the AI signal at the
	// moment of OPEN and NEVER recomputed from later AI evaluations —
	// rule is structural: the position monitor needs a stable plan to
	// evaluate against, and the audit trail must be able to answer
	// "what was the exit plan when this trade opened?" from this row
	// alone, without reconstructing from audit_logs.
	//
	// Nullable: a position created before CRY-16 (legacy data) or one
	// where the AI declined to provide SL/TP simply has nil here. As of
	// CRY-22 the monitor evaluates every is_open=true row regardless of
	// SL/TP, so a position without an immutable plan still gets trailing
	// stop protection.
	StopLoss   *decimal.Decimal `gorm:"type:numeric(20,8)" json:"stop_loss,omitempty"`
	TakeProfit *decimal.Decimal `gorm:"type:numeric(20,8)" json:"take_profit,omitempty"`

	// CRY-22 trailing-stop state. PeakPrice is updated by the monitor on
	// every tick to max(PeakPrice, current_price); MaxUnrealizedPnLPct
	// tracks the best unrealized PnL seen since open. TrailingStopArmed
	// latches true once MaxUnrealizedPnLPct crosses TRAILING_STOP_ARM_PCT
	// (default 1%) — once armed it stays armed, so a position that briefly
	// touched +1% then settled back to +0.5% still trails.
	//
	// All three are nullable / default-false so existing OPEN positions
	// migrate cleanly; BackfillPositionPeaks (cmd/server bootstrap)
	// populates them from candle history.
	//
	// Explicit `column:max_unrealized_pnl_pct` for the same PnL → pn_l
	// GORM naming-bug workaround the rest of this file uses.
	PeakPrice           *decimal.Decimal `gorm:"type:numeric(20,8)"                                            json:"peak_price,omitempty"`
	MaxUnrealizedPnLPct *decimal.Decimal `gorm:"column:max_unrealized_pnl_pct;type:numeric(10,4)"              json:"max_unrealized_pnl_pct,omitempty"`
	TrailingStopArmed   bool             `gorm:"not null;default:false"                                        json:"trailing_stop_armed"`
}

func (Position) TableName() string { return "positions" }
