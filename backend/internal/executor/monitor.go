package executor

import (
	"context"
	"fmt"
	"log"
	"time"

	"cryptobot/internal/database/models"
	"cryptobot/internal/marketprice"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// PositionMonitor is the deterministic SL/TP exit engine. It ticks at a
// short cadence (default 5s) reading every OPEN position with a non-nil
// risk snapshot, evaluates the freshest known market price (from
// marketprice.Store — NOT a candle DB poll), and triggers an atomic
// closePosition on hit.
//
// CRY-16 introduces this as a structural component of the trading
// lifecycle: an exit must NOT depend on the AI returning a position-
// aware SELL. The monitor is the safety net that guarantees the bot
// honours the risk plan it announced at OPEN, regardless of model
// behavior.
//
// Concurrency:
//   - Monitor + AI SELL on the same tick → closePosition's CAS resolves
//     the race. Whichever side wins records the close; the loser sees
//     ErrAlreadyClosed and silently no-ops.
//   - Monitor + monitor on the same position (overlapping ticks) →
//     same CAS guard. Only one tick closes the position; subsequent
//     ticks see no OPEN row to process.
type PositionMonitor struct {
	db                 *gorm.DB
	exec               *Executor
	prices             *marketprice.Store
	interval           time.Duration
	maxPriceAge        time.Duration
	trailingStopPct    float64 // CRY-22 — 0 disables trailing-stop firing
	trailingStopArmPct float64
	minLockedProfitPct float64
	noPriceLogAt       map[string]time.Time // throttled per-symbol "no price" warnings
}

// MonitorConfig packages the operator-tunable knobs.
type MonitorConfig struct {
	// Interval between sweeps. Defaults to 5s if zero. Clamped to
	// [1s, 60s] — sub-second wastes CPU on the RWMutex; >60s leaves
	// the bot exposed for a candle worth of adverse move.
	Interval time.Duration
	// MaxPriceAge — quotes older than this are treated as missing.
	// Default 60s. The WS feed publishes every 1s when alive, so a
	// stale entry means the feed dropped for at least one keepalive
	// cycle and we should refuse to act on it.
	MaxPriceAge time.Duration

	// CRY-22 trailing-stop knobs. See config.Config docs for full semantics.
	// All three default to 0 here — zero TrailingStopPct disables firing
	// (the monitor still tracks peak_price for observability).
	TrailingStopPct    float64
	TrailingStopArmPct float64
	MinLockedProfitPct float64
}

// NewPositionMonitor wires the monitor to its dependencies. The exec
// pointer is required because closePosition + the audit-log writer
// hang off Executor; the monitor never duplicates that logic.
func NewPositionMonitor(db *gorm.DB, exec *Executor, prices *marketprice.Store, cfg MonitorConfig) *PositionMonitor {
	interval := cfg.Interval
	switch {
	case interval <= 0:
		interval = 5 * time.Second
	case interval < time.Second:
		interval = time.Second
	case interval > 60*time.Second:
		interval = 60 * time.Second
	}
	maxAge := cfg.MaxPriceAge
	if maxAge <= 0 {
		maxAge = 60 * time.Second
	}
	return &PositionMonitor{
		db:                 db,
		exec:               exec,
		prices:             prices,
		interval:           interval,
		maxPriceAge:        maxAge,
		trailingStopPct:    cfg.TrailingStopPct,
		trailingStopArmPct: cfg.TrailingStopArmPct,
		minLockedProfitPct: cfg.MinLockedProfitPct,
		noPriceLogAt:       make(map[string]time.Time),
	}
}

// Run is the blocking sweep loop. Cancel ctx to stop. Designed to be
// launched via main.go's supervise() so a panic in evaluateOnce restarts
// the loop with a short backoff.
func (m *PositionMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	log.Printf("[monitor] started — interval=%s max_price_age=%s", m.interval, m.maxPriceAge)
	for {
		select {
		case <-ctx.Done():
			log.Println("[monitor] stopped")
			return
		case <-ticker.C:
			m.evaluateOnce(ctx)
		}
	}
}

// evaluateOnce is one sweep: load every OPEN position (regardless of
// SL/TP), fetch the freshest price, update trailing state, and fire a
// close on hit. Pulled out for testability — tests inject a synthetic
// price in marketprice.Store and call evaluateOnce directly.
//
// CRY-22 widened the WHERE from `(stop_loss IS NOT NULL OR take_profit
// IS NOT NULL)` to plain `is_open = true` so positions without a fixed
// SL/TP plan still get trailing-stop protection. Pre-CRY-22 the filter
// silently excluded any position the AI declined to set TP on.
func (m *PositionMonitor) evaluateOnce(ctx context.Context) {
	var positions []models.Position
	if err := m.db.WithContext(ctx).
		Where("is_open = ?", true).
		Find(&positions).Error; err != nil {
		log.Printf("[monitor] load positions: %v", err)
		return
	}
	for _, pos := range positions {
		m.evaluatePosition(ctx, pos)
	}
}

// evaluatePosition checks one position against the freshest price.
// Every tick it (a) refreshes peak_price / max_unrealized_pnl_pct /
// trailing_stop_armed on the DB row, then (b) evaluates the canonical
// exit hierarchy STOP_LOSS > TRAILING_STOP > TAKE_PROFIT and fires
// closePosition on the first match.
//
// A missing or stale price short-circuits before any DB write — we
// neither close nor update tracking on stale data, because the
// monitor's whole point is to react to current market state.
//
// Long-only for now. The current bot only opens BUY (long) positions;
// SHORT support would invert the comparisons here. Marked TODO so the
// next reader doesn't have to re-derive that the logic is intentional.
func (m *PositionMonitor) evaluatePosition(ctx context.Context, pos models.Position) {
	quote, ok := m.prices.Get(pos.Symbol)
	if !ok {
		m.logNoPrice(pos.Symbol, "no quote in store")
		return
	}
	if quote.IsStale(m.maxPriceAge) {
		m.logNoPrice(pos.Symbol, fmt.Sprintf("quote stale (age=%s, max=%s)", time.Since(quote.At), m.maxPriceAge))
		return
	}

	// CRY-22 trailing state update — runs every tick, including when no
	// rule will fire. Peak_price + max_unrealized_pnl_pct are useful for
	// the audit columns in PR-2 and the prompt enrichment in PR-3 even
	// when trailing-stop firing is disabled.
	newPeak, armed, trailFired := evaluateTrailingStop(
		pos, quote.Price,
		m.trailingStopPct, m.trailingStopArmPct, m.minLockedProfitPct,
		m.exec.feeRate,
	)
	m.persistTrailingState(ctx, pos, newPeak, armed)

	// Canonical hierarchy: STOP_LOSS > TRAILING_STOP > TAKE_PROFIT.
	// Hard exits always take precedence.
	reason, fired := evaluateCombinedExit(pos, quote.Price, trailFired)
	if !fired {
		return
	}
	// CRY-30: live exits sell on the venue and back off after a failure;
	// skip while one is in flight or backing off so each attempt — not
	// each tick — gets an audit row.
	if !m.exec.exitReady(pos, time.Now()) {
		return
	}

	// Synthesize an audit_log row so the operator's audit trail captures
	// "monitor closed this — here is why, here was the price". The
	// inference-time / prompt fields are intentionally synthetic; the
	// reasoning string carries the structured signal.
	auditID := m.writeMonitorAuditLog(ctx, pos, quote, reason, newPeak)

	// Paper closes at the quote; live sells on the venue first (CRY-30).
	tradeID, err := m.exec.exitPosition(ctx, pos, quote.Price, reason, auditID)
	if err == ErrAlreadyClosed {
		log.Printf("[monitor] sym=%s already closed by another path — no-op", pos.Symbol)
		return
	}
	if err != nil {
		log.Printf("[monitor] close error sym=%s reason=%s: %v", pos.Symbol, reason, err)
		return
	}
	log.Printf("[monitor] %s fired: sym=%s price=%s sl=%s tp=%s peak=%s trade_id=%d",
		reason, pos.Symbol, quote.Price, fmtPtr(pos.StopLoss), fmtPtr(pos.TakeProfit), newPeak, tradeID)
}

// persistTrailingState writes peak_price, max_unrealized_pnl_pct, and
// trailing_stop_armed to the position row when any of them changed.
// Single UPDATE per tick; no-op when nothing moved.
//
// Gated on is_open=true so a concurrent close that flipped is_open to
// false between load and update doesn't resurrect the row's state.
func (m *PositionMonitor) persistTrailingState(ctx context.Context, pos models.Position, newPeak decimal.Decimal, armed bool) {
	newMaxPnLPct := decimal.Zero
	if !pos.AvgEntryPrice.IsZero() {
		newMaxPnLPct = newPeak.Sub(pos.AvgEntryPrice).Div(pos.AvgEntryPrice).Mul(decimal.NewFromInt(100))
	}
	if pos.MaxUnrealizedPnLPct != nil && pos.MaxUnrealizedPnLPct.GreaterThan(newMaxPnLPct) {
		newMaxPnLPct = *pos.MaxUnrealizedPnLPct
	}
	updates := map[string]any{}
	if pos.PeakPrice == nil || !pos.PeakPrice.Equal(newPeak) {
		updates["peak_price"] = newPeak
	}
	if pos.MaxUnrealizedPnLPct == nil || !pos.MaxUnrealizedPnLPct.Equal(newMaxPnLPct) {
		updates["max_unrealized_pnl_pct"] = newMaxPnLPct
	}
	if armed && !pos.TrailingStopArmed {
		updates["trailing_stop_armed"] = true
	}
	if len(updates) == 0 {
		return
	}
	if err := m.db.WithContext(ctx).Model(&models.Position{}).
		Where("id = ? AND is_open = ?", pos.ID, true).
		Updates(updates).Error; err != nil {
		log.Printf("[monitor] WARN: trailing-state update failed sym=%s id=%d: %v", pos.Symbol, pos.ID, err)
	}
}

// evaluateExitRules is the pure-function rule engine for the SL/TP-only
// subset of the exit hierarchy — broken out for table-driven tests.
// Returns (TAKE_PROFIT / STOP_LOSS, true) on hit, or ("", false) when
// neither rule fired.
//
// CRY-22: this function is still here for the focused SL/TP unit tests
// in monitor_test.go (TestEvaluateExitRules_*). The runtime call site
// has moved to evaluateCombinedExit, which composes this with the
// trailing-stop rule under the canonical hierarchy.
//
// Long-only semantics (see evaluatePosition for the rationale):
//   - price ≤ StopLoss  → STOP_LOSS
//   - price ≥ TakeProfit → TAKE_PROFIT
//   - StopLoss precedence when both fire on a single quote
func evaluateExitRules(pos models.Position, price decimal.Decimal) (models.ExitReason, bool) {
	if pos.StopLoss != nil && price.LessThanOrEqual(*pos.StopLoss) {
		return models.ExitReasonStopLoss, true
	}
	if pos.TakeProfit != nil && price.GreaterThanOrEqual(*pos.TakeProfit) {
		return models.ExitReasonTakeProfit, true
	}
	return "", false
}

// evaluateCombinedExit applies the canonical CRY-22 exit hierarchy:
//
//	STOP_LOSS > TRAILING_STOP > TAKE_PROFIT
//
// Hard exits always take precedence. trailFired is computed by the
// caller via evaluateTrailingStop so this function stays a pure
// composition of the simpler rule funcs and remains testable without
// fee-rate injection.
func evaluateCombinedExit(pos models.Position, price decimal.Decimal, trailFired bool) (models.ExitReason, bool) {
	if pos.StopLoss != nil && price.LessThanOrEqual(*pos.StopLoss) {
		return models.ExitReasonStopLoss, true
	}
	if trailFired {
		return models.ExitReasonTrailingStop, true
	}
	if pos.TakeProfit != nil && price.GreaterThanOrEqual(*pos.TakeProfit) {
		return models.ExitReasonTakeProfit, true
	}
	return "", false
}

// evaluateTrailingStop is the pure-function trailing-stop rule. It
// always recomputes the new peak (peak tracking is useful for audit /
// prompt context even when firing is disabled), then layers three
// independent conditions before signaling a close:
//
//  1. Trailing must be enabled: trailPct > 0.
//  2. Trail must be armed: peak unrealized PnL % ≥ armPct. Latches via
//     pos.TrailingStopArmed so a position that briefly touched the arm
//     threshold then settled back still trails.
//  3. Drawdown threshold: price ≤ newPeak × (1 − trailPct/100).
//  4. Net-locked-profit gate: the close must lock in ≥ minLockedProfitPct
//     net post-fee return (fees mirrored from executor.closePosition via
//     the executor's shared feeRate). Prevents micro-profit churn when a
//     position is armed by the peak but the current price would only
//     lock in <0.5% net.
//
// Returns (newPeak, armed, fired). Callers persist newPeak / armed
// every tick; only `fired` drives a close.
func evaluateTrailingStop(
	pos models.Position, price decimal.Decimal,
	trailPct, armPct, minLockedProfitPct float64, feeRate decimal.Decimal,
) (newPeak decimal.Decimal, armed, fired bool) {
	// Bootstrap peak from entry if NULL (e.g. backfill hasn't run on this
	// row yet, or it was just opened pre-tick).
	peakStart := pos.AvgEntryPrice
	if pos.PeakPrice != nil {
		peakStart = *pos.PeakPrice
	}
	newPeak = peakStart
	if price.GreaterThan(peakStart) {
		newPeak = price
	}

	// Best unrealized PnL % since open (using the persisted max if it
	// exceeds what the current peak implies — handles the boot-backfill
	// case where peak may have come from candle history).
	peakPnLPct := decimal.Zero
	if !pos.AvgEntryPrice.IsZero() {
		peakPnLPct = newPeak.Sub(pos.AvgEntryPrice).Div(pos.AvgEntryPrice).Mul(decimal.NewFromInt(100))
	}
	if pos.MaxUnrealizedPnLPct != nil && pos.MaxUnrealizedPnLPct.GreaterThan(peakPnLPct) {
		peakPnLPct = *pos.MaxUnrealizedPnLPct
	}

	// Trailing disabled — still track peak / armed, but never fire.
	if trailPct <= 0 {
		return newPeak, pos.TrailingStopArmed, false
	}

	armed = pos.TrailingStopArmed || peakPnLPct.GreaterThanOrEqual(decimal.NewFromFloat(armPct))
	if !armed {
		return newPeak, false, false
	}

	trailThreshold := newPeak.Mul(decimal.NewFromInt(1).Sub(decimal.NewFromFloat(trailPct).Div(decimal.NewFromInt(100))))
	if price.GreaterThan(trailThreshold) {
		return newPeak, true, false
	}

	netPct := netLockedPnLPct(pos.AvgEntryPrice, price, pos.Quantity, feeRate)
	if netPct.LessThan(decimal.NewFromFloat(minLockedProfitPct)) {
		return newPeak, true, false
	}
	return newPeak, true, true
}

// netLockedPnLPct mirrors the round-trip fee accounting in
// executor.closePosition: entry fee + exit fee both charged at feeRate
// against their respective notionals, net PnL expressed as a % of
// entry cost. Used by evaluateTrailingStop's micro-profit gate.
func netLockedPnLPct(entry, exit, qty, feeRate decimal.Decimal) decimal.Decimal {
	if entry.IsZero() || qty.IsZero() {
		return decimal.Zero
	}
	entryCost := entry.Mul(qty)
	exitProceeds := exit.Mul(qty)
	entryFee := entryCost.Mul(feeRate)
	exitFee := exitProceeds.Mul(feeRate)
	netPnL := exitProceeds.Sub(entryCost).Sub(entryFee).Sub(exitFee)
	return netPnL.Div(entryCost).Mul(decimal.NewFromInt(100))
}

// writeMonitorAuditLog records the synthetic audit_log row that
// represents a monitor-triggered close. Standard, not bandaid: every
// trading-lifecycle event has an audit row, including ones the AI did
// not produce. Returns the new row id so closePosition can patch
// trade_id back in.
//
// For TRAILING_STOP rows the reasoning string is augmented with the
// peak price, drawdown % from peak, and net-locked PnL % so the
// operator can audit "why did this fire here?" without re-deriving
// from peak_price + current quote.
func (m *PositionMonitor) writeMonitorAuditLog(ctx context.Context, pos models.Position, quote marketprice.Quote, reason models.ExitReason, newPeak decimal.Decimal) uint {
	reasoning := fmt.Sprintf("auto: %s — price=%s sl=%s tp=%s", reason, quote.Price, fmtPtr(pos.StopLoss), fmtPtr(pos.TakeProfit))
	if reason == models.ExitReasonTrailingStop && !newPeak.IsZero() {
		drawdownPct := decimal.Zero
		if !newPeak.IsZero() {
			drawdownPct = newPeak.Sub(quote.Price).Div(newPeak).Mul(decimal.NewFromInt(100))
		}
		lockedNetPct := netLockedPnLPct(pos.AvgEntryPrice, quote.Price, pos.Quantity, m.exec.feeRate)
		reasoning = fmt.Sprintf("auto: TRAILING_STOP — peak=%s drawdown=%s%% locked_net=%s%%",
			newPeak.StringFixed(8), drawdownPct.StringFixed(4), lockedNetPct.StringFixed(4))
	}
	entry := models.AuditLog{
		Symbol:       pos.Symbol,
		Interval:     "monitor", // distinguishes from AI-produced rows
		Prompt:       "monitor: deterministic rule evaluation (SL / TRAILING / TP)",
		MarketPrice:  quote.Price,
		RawResponse:  fmt.Sprintf(`{"action":"SELL","reason":%q}`, reason),
		Action:       "SELL",
		Confidence:   1.0, // monitor decisions are deterministic, not probabilistic
		Reasoning:    reasoning,
		Executed:     false, // patched by closePosition.linkAuditLog
		PromptSentAt: quote.At,
		ResponseAt:   time.Now(),
	}
	if err := m.db.WithContext(ctx).Create(&entry).Error; err != nil {
		log.Printf("[monitor] WARN: audit_log write failed sym=%s: %v", pos.Symbol, err)
		return 0
	}
	return entry.ID
}

// logNoPrice throttles "no price" warnings to once per minute per
// symbol so a missing feed cannot flood the log.
func (m *PositionMonitor) logNoPrice(symbol, detail string) {
	now := time.Now()
	last, ok := m.noPriceLogAt[symbol]
	if ok && now.Sub(last) < time.Minute {
		return
	}
	m.noPriceLogAt[symbol] = now
	log.Printf("[monitor] sym=%s skipped: %s", symbol, detail)
}

// fmtPtr renders a *decimal.Decimal as either its string value or
// "<nil>" for log lines.
func fmtPtr(d *decimal.Decimal) string {
	if d == nil {
		return "<nil>"
	}
	return d.String()
}
