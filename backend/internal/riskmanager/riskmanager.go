package riskmanager

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"cryptobot/internal/database/models"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Config holds all risk parameters. Loaded from environment or defaults.
type Config struct {
	MaxPositionPct  float64 // max % of equity per trade (default 2%)
	DailyDrawdownPct float64 // halt threshold — max daily loss as % of starting equity (default 5%)
	MaxOpenPositions int     // max simultaneous open positions (default 3)
	// PrioritySymbol reserves the LAST open-position slot for one
	// designated symbol (default ETHUSDT). When set, a BUY for any
	// other symbol is blocked once open positions reach
	// MaxOpenPositions-1, unless the priority symbol already has an
	// open position. Empty string disables the gate — back-compatible
	// with deployments that don't set PRIORITY_SYMBOL.
	PrioritySymbol string
	// BaseCapital is the starting equity that realized net PnL is added
	// to. Zero means DefaultBaseCapital — the placeholder until equity is
	// read from the wallet balance.
	BaseCapital decimal.Decimal
}

// DefaultBaseCapital is the paper starting balance (USDT).
var DefaultBaseCapital = decimal.NewFromInt(10000)

func DefaultConfig() Config {
	return Config{
		MaxPositionPct:   2.0,
		DailyDrawdownPct: 5.0,
		MaxOpenPositions: 3,
		PrioritySymbol:   "",
	}
}

// Breach describes why a risk check failed.
type Breach struct {
	Rule   string // e.g. "daily_drawdown", "max_position", "max_open_positions"
	Detail string
}

func (b Breach) Error() string {
	return fmt.Sprintf("risk breach [%s]: %s", b.Rule, b.Detail)
}

// RiskManager sits between the AI signal and the executor.
// It enforces hard limits on capital exposure and daily losses.
type RiskManager struct {
	db  *gorm.DB
	cfg Config

	mu     sync.RWMutex
	halted bool // set to true when daily drawdown is breached
	// haltedDay is the UTC day the breaker tripped. The breaker is a
	// DAILY limit, so it clears itself once that day is over.
	haltedDay time.Time
}

func New(db *gorm.DB, cfg Config) *RiskManager {
	return &RiskManager{db: db, cfg: cfg}
}

// IsHalted returns true if the circuit breaker has tripped today (UTC).
// A trip from a previous day is cleared on read.
func (rm *RiskManager) IsHalted() bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.halted && rm.haltedDay.Before(todayUTC()) {
		rm.halted = false
		log.Println("[risk] circuit breaker auto-reset — new UTC day")
	}
	return rm.halted
}

// Reset clears the halted flag (manual override).
func (rm *RiskManager) Reset() {
	rm.mu.Lock()
	rm.halted = false
	rm.mu.Unlock()
	log.Println("[risk] circuit breaker reset")
}

// Equity returns base capital plus the sum of NET realized PnL over all
// closed trades — the same figure the portfolio view shows, so the
// drawdown breaker and position sizing both see fee drag.
func (rm *RiskManager) Equity() decimal.Decimal {
	base := rm.cfg.BaseCapital
	if base.IsZero() {
		base = DefaultBaseCapital
	}
	var totalStr string
	rm.db.Model(&models.Trade{}).
		Select("COALESCE(SUM(net_pnl), 0)").
		Where("status = ?", models.TradeStatusClosed).
		Row().Scan(&totalStr)
	total, _ := decimal.NewFromString(totalStr)
	return base.Add(total)
}

// PreTradeCheck runs all risk checks before a new trade is opened.
// Returns nil if the trade is allowed, or a Breach error.
func (rm *RiskManager) PreTradeCheck(symbol string, side models.TradeSide, equity decimal.Decimal) error {
	if rm.IsHalted() {
		return Breach{Rule: "circuit_breaker", Detail: "trading halted — daily drawdown limit exceeded"}
	}

	// ── Check 1: Daily drawdown ──────────────────────────
	if err := rm.checkDailyDrawdown(equity); err != nil {
		return err
	}

	// ── Check 2: Max open positions ──────────────────────
	if side == models.TradeSideBuy {
		var openCount int64
		rm.db.Model(&models.Position{}).Where("is_open = ?", true).Count(&openCount)
		if int(openCount) >= rm.cfg.MaxOpenPositions {
			return Breach{
				Rule:   "max_open_positions",
				Detail: fmt.Sprintf("already at %d open positions (limit %d)", openCount, rm.cfg.MaxOpenPositions),
			}
		}

		// ── Check 3: Priority-symbol slot reservation ────────
		// When configured, hold the LAST slot open for the priority
		// symbol so a fully-engaged 3-position book can still admit
		// the operator's designated lead. Gate only fires for the
		// final slot — earlier BUYs are unaffected. Once the priority
		// symbol is itself open the reservation is satisfied and
		// any subsequent BUY can take the last slot.
		if rm.cfg.PrioritySymbol != "" && symbol != rm.cfg.PrioritySymbol {
			slotsLeft := rm.cfg.MaxOpenPositions - int(openCount)
			if slotsLeft == 1 {
				var priorityOpen int64
				rm.db.Model(&models.Position{}).
					Where("is_open = ? AND symbol = ?", true, rm.cfg.PrioritySymbol).
					Count(&priorityOpen)
				if priorityOpen == 0 {
					return Breach{
						Rule: "priority_slot_reserved",
						Detail: fmt.Sprintf(
							"last open slot reserved for %s (open=%d/%d, %s not yet open)",
							rm.cfg.PrioritySymbol, openCount, rm.cfg.MaxOpenPositions, rm.cfg.PrioritySymbol),
					}
				}
			}
		}
	}

	return nil
}

// ComputePositionSize returns the quantity to buy given equity and current price,
// capped at MaxPositionPct of equity.
func (rm *RiskManager) ComputePositionSize(equity, price decimal.Decimal) decimal.Decimal {
	maxAllocation := equity.Mul(decimal.NewFromFloat(rm.cfg.MaxPositionPct / 100.0))
	if price.IsZero() {
		return decimal.Zero
	}
	return maxAllocation.Div(price).Round(8)
}

// PostTradeCheck evaluates portfolio state after a trade closes.
// If the daily drawdown threshold is breached, it trips the circuit breaker.
func (rm *RiskManager) PostTradeCheck(equity decimal.Decimal) {
	if err := rm.checkDailyDrawdown(equity); err != nil {
		rm.mu.Lock()
		rm.halted = true
		rm.haltedDay = todayUTC()
		rm.mu.Unlock()
		log.Printf("[risk] CIRCUIT BREAKER TRIPPED: %v", err)
	}
}

// dailyNetPnL sums NET realized PnL (fees included) of trades closed
// since UTC midnight.
func (rm *RiskManager) dailyNetPnL() decimal.Decimal {
	var totalStr string
	rm.db.Model(&models.Trade{}).
		Select("COALESCE(SUM(net_pnl), 0)").
		Where("status = ? AND exit_at >= ?", models.TradeStatusClosed, todayUTC()).
		Row().Scan(&totalStr)
	total, _ := decimal.NewFromString(totalStr)
	return total
}

// dailyLossPct is today's net realized loss as a percentage of the
// equity at the START of the day (current equity minus today's net
// PnL). Returns 0 when today is flat or profitable. Single formula for
// both the breaker and the dashboard status.
func dailyLossPct(dailyNet, equity decimal.Decimal) decimal.Decimal {
	if !dailyNet.IsNegative() {
		return decimal.Zero
	}
	startEquity := equity.Sub(dailyNet)
	if !startEquity.IsPositive() {
		return decimal.Zero
	}
	return dailyNet.Abs().Div(startEquity).Mul(decimal.NewFromInt(100))
}

// checkDailyDrawdown compares today's net loss against the threshold.
func (rm *RiskManager) checkDailyDrawdown(equity decimal.Decimal) error {
	dailyNet := rm.dailyNetPnL()
	lossPct := dailyLossPct(dailyNet, equity)
	if lossPct.IsPositive() && lossPct.GreaterThanOrEqual(decimal.NewFromFloat(rm.cfg.DailyDrawdownPct)) {
		return Breach{
			Rule: "daily_drawdown",
			Detail: fmt.Sprintf(
				"daily loss %.2f%% exceeds limit %.2f%% (net PnL: %s, start-of-day equity: %s)",
				lossPct.InexactFloat64(), rm.cfg.DailyDrawdownPct,
				dailyNet.StringFixed(2), equity.Sub(dailyNet).StringFixed(2),
			),
		}
	}
	return nil
}

// Status returns a snapshot of risk state for the dashboard API.
type Status struct {
	Halted bool `json:"halted"`
	// DailyPnL is today's NET realized PnL (USDT, fees included).
	DailyPnL decimal.Decimal `json:"daily_pnl"`
	// DailyDrawdownPct is today's net loss as a percent of start-of-day
	// equity — the exact number the breaker compares to DailyLimitPct.
	// 0 when today is flat or profitable.
	DailyDrawdownPct float64 `json:"daily_drawdown_pct"`
	OpenPositions    int64   `json:"open_positions"`
	MaxOpenPositions int     `json:"max_open_positions"`
	MaxPositionPct   float64 `json:"max_position_pct"`
	DailyLimitPct    float64 `json:"daily_limit_pct"`
	// PrioritySymbol echoes the configured reservation target (empty
	// string when the gate is disabled). PriorityReserved is true
	// when the gate is *currently* withholding the last slot — i.e.
	// the priority symbol has no open position AND the book is one
	// short of the cap. Dashboard surfaces both so an operator can
	// see at a glance why a non-priority BUY signal was rejected.
	PrioritySymbol   string `json:"priority_symbol"`
	PriorityReserved bool   `json:"priority_reserved"`
}

func (rm *RiskManager) GetStatus() Status {
	dailyNet := rm.dailyNetPnL()
	lossPct := dailyLossPct(dailyNet, rm.Equity())

	var openCount int64
	rm.db.Model(&models.Position{}).Where("is_open = ?", true).Count(&openCount)

	priorityReserved := false
	if rm.cfg.PrioritySymbol != "" {
		slotsLeft := rm.cfg.MaxOpenPositions - int(openCount)
		if slotsLeft == 1 {
			var priorityOpen int64
			rm.db.Model(&models.Position{}).
				Where("is_open = ? AND symbol = ?", true, rm.cfg.PrioritySymbol).
				Count(&priorityOpen)
			priorityReserved = priorityOpen == 0
		}
	}

	return Status{
		Halted:           rm.IsHalted(),
		DailyPnL:         dailyNet,
		DailyDrawdownPct: lossPct.InexactFloat64(),
		OpenPositions:    openCount,
		MaxOpenPositions: rm.cfg.MaxOpenPositions,
		MaxPositionPct:   rm.cfg.MaxPositionPct,
		DailyLimitPct:    rm.cfg.DailyDrawdownPct,
		PrioritySymbol:   rm.cfg.PrioritySymbol,
		PriorityReserved: priorityReserved,
	}
}

func todayUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// MaxStopLossGate (CRY-37) blocks a BUY whose stop-loss sits more than
// capPct percent below the signal price. The stop distance is the loss the
// trade accepts before it is cut; CRY-36 found the widest stops drove most
// of the realised loss. Shared by the live executor and the backtester.
//
//   - capPct ≤ 0                 → gate disabled, never blocks
//   - stopLoss ≤ 0               → admitted (AI declined a stop; same as the
//     wrong-side gate, the monitor then has no stop to run)
//   - currentPrice ≤ 0           → block (can't compute the distance)
//   - stopLoss ≥ currentPrice    → not this gate's concern (wrong-side gate)
//
// A stop exactly at the cap is admitted; a 1e-9 tolerance absorbs float
// noise in the percentage.
func MaxStopLossGate(stopLoss, currentPrice, capPct float64) (blocked bool, distPct float64) {
	if capPct <= 0 || stopLoss <= 0 {
		return false, 0
	}
	if currentPrice <= 0 {
		return true, 0
	}
	distPct = (currentPrice - stopLoss) / currentPrice * 100
	return distPct > capPct+1e-9, distPct
}

// MinExpectedReturnGate decides whether a BUY signal's TakeProfit clears
// the configured minimum-return floor. The floor exists so the bot does
// not enter trades whose own AI-projected upside is so thin that a single
// round-trip fee (0.2% on Binance Spot taker, BNB discount off) would erase it.
//
// Returns blocked=true when the signal must be rejected and the computed
// bps for logging. Pure function — no state, no DB — so the backtester
// can reuse the exact same gate the live executor applies. Standard,
// not a bandaid: a shared seam beats two parallel formulas drifting apart.
//
// Defensive rules collapse to a single block outcome:
//   - currentPrice ≤ 0           → block (can't compute bps)
//   - takeProfit ≤ 0             → block (AI didn't supply a target)
//   - takeProfit ≤ currentPrice  → block (AI's own target says no upside)
//   - capBps ≤ 0                 → gate disabled, never blocks
//
// math.Round (not int64 truncation) so a signal whose intent is exactly
// the cap (e.g. 30 bps) is admitted instead of rounded down to 29.
func MinExpectedReturnGate(takeProfit, currentPrice float64, capBps int64) (blocked bool, bps int64) {
	if capBps <= 0 {
		return false, 0
	}
	if currentPrice <= 0 || takeProfit <= 0 || takeProfit <= currentPrice {
		return true, 0
	}
	bps = int64(math.Round((takeProfit - currentPrice) / currentPrice * 10000))
	return bps < capBps, bps
}
