package executor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"cryptobot/internal/approval"
	"cryptobot/internal/database/models"
	"cryptobot/internal/execution"
	"cryptobot/internal/riskmanager"
	"cryptobot/internal/strategy"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Mode names the three execution behaviours. Standard, not a bandaid:
// the three paths are explicit so a config typo cannot silently route
// signals into live execution.
//
//   - ModeDryRun: signals are evaluated, risk-checked, but no Trade row
//     is created. Useful for backtest-style observation.
//   - ModePaper: signals open simulated Trade rows (status OPEN with
//     placeholder fill price) — same path as ModeDryRun but state moves.
//     Used by CRY-12 paper-trade-live validation.
//   - ModeLive: signals are enqueued into pending_signals; only a
//     human approval (via the approval state machine) can produce a
//     Trade row.
type Mode string

const (
	ModeDryRun Mode = "dry-run"
	ModePaper  Mode = "paper"
	ModeLive   Mode = "live"
)

// ParseMode is the single source of truth for the mode string. Returns
// an error rather than defaulting silently so config typos fail fast.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeDryRun, ModePaper, ModeLive:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("invalid execution mode %q", s)
	}
}

// Notifier is the WS-broadcast contract used to push `signal.pending`
// events to dashboards. Tests pass nil; production passes the hub.
type Notifier interface {
	Broadcast(map[string]any)
}

// Executor evaluates AI signals and routes them through the appropriate
// path for the current Mode. It is safe to call Submit concurrently with
// the Run loop — Submit owns its own DB transactions and the position
// lookup is single-row.
type Executor struct {
	db       *gorm.DB
	analyzer *strategy.Analyzer
	risk     *riskmanager.RiskManager
	mode     Mode

	// approval is non-nil iff mode == ModeLive. The executor never
	// touches it in other modes, and main.go never constructs it.
	approval *approval.StateMachine

	// notifier may be nil; broadcasts are nil-safe.
	notifier Notifier

	// provider is the Binance execution adapter — non-nil in ModeLive,
	// nil elsewhere. Submit refuses to run if live mode + nil provider.
	provider execution.ExecutionProvider

	// maxSlippageBps caps the allowed drift between expected (signal
	// snapshot) and actual fill price. 0 disables the check.
	maxSlippageBps int64

	// minExpectedReturnBps blocks BUY signals whose distance from
	// MarketPrice to TakeProfit (in basis points) falls below this.
	// 0 disables. CRY-9 default 30 = 0.3% — enough to clear a
	// round-trip Binance Spot fee (0.1% × 2 = 0.2%) plus some
	// slippage room without leaving a margin so thin that fee noise
	// dominates outcomes.
	minExpectedReturnBps int64

	// maxStopLossPct blocks BUY signals whose stop-loss sits more than
	// this percent below the signal price (CRY-37). 0 disables.
	maxStopLossPct float64

	// feeRate is the simulated per-leg taker fee for paper / dry-run
	// fills and the monitor's net-locked-profit gate. Defaults to
	// DefaultPaperFeeRate; override with WithPaperFeeRate.
	feeRate decimal.Decimal

	// exits serialises live SELLs per symbol (monitor exits and approved
	// AI SELLs) and holds monitor-exit retry state. CRY-30.
	exits *exitTracker
}

// DefaultPaperFeeRate is the simulated Binance Spot taker rate used in
// paper and dry-run modes. 0.1% — the standard rate with "Use BNB for
// fees" OFF, which is what live mode requires (CRY-9 runbook), so paper
// PnL books the same fee drag live would.
var DefaultPaperFeeRate = decimal.RequireFromString("0.001")

// ErrAlreadyClosed is returned by closePosition when the CAS UPDATE
// affected zero rows — either the position never existed or another
// caller (monitor vs. AI SELL race) won the close. Callers treat this
// as a benign no-op, NOT an error: the desired end state (position
// closed exactly once) is already true.
//
// CRY-16: this is the structural invariant that makes the trading
// lifecycle safe under concurrent inference + monitor activity.
var ErrAlreadyClosed = errors.New("position already closed")

// NewExecutor constructs an Executor for the given Mode. When mode is
// ModeLive, the approval state machine is required; in dry-run / paper
// it must be nil (caller assertion enforced at construction).
func NewExecutor(
	db *gorm.DB,
	analyzer *strategy.Analyzer,
	risk *riskmanager.RiskManager,
	mode Mode,
	sm *approval.StateMachine,
) *Executor {
	if mode == ModeLive && sm == nil {
		log.Fatal("executor: ModeLive requires a non-nil StateMachine")
	}
	if mode != ModeLive && sm != nil {
		log.Fatal("executor: non-live modes must not receive a StateMachine")
	}
	return &Executor{db: db, analyzer: analyzer, risk: risk, mode: mode, approval: sm, feeRate: DefaultPaperFeeRate, exits: newExitTracker()}
}

// WithPaperFeeRate overrides the simulated per-leg fee rate (e.g. 0.001
// = 0.1%). Non-positive values are ignored.
func (e *Executor) WithPaperFeeRate(rate decimal.Decimal) *Executor {
	if rate.IsPositive() {
		e.feeRate = rate
	}
	return e
}

// WithNotifier attaches a WS notifier post-construction so main.go can
// build the hub and executor in any order.
func (e *Executor) WithNotifier(n Notifier) *Executor {
	e.notifier = n
	return e
}

// WithProvider attaches the Binance execution adapter and slippage cap.
// Required in ModeLive (Submit returns an error otherwise). Standard,
// not a bandaid: dry-run and paper modes never receive a provider so a
// configuration mix-up cannot route fake signals into the live wire.
func (e *Executor) WithProvider(p execution.ExecutionProvider, maxSlippageBps int64) *Executor {
	e.provider = p
	e.maxSlippageBps = maxSlippageBps
	return e
}

// WithMinReturnGate attaches the CRY-9 min-expected-return threshold.
// Applied in route() before mode dispatch, so it filters Live (no
// pending_signals row created) and Paper / DryRun (no Trade row
// created) identically. 0 disables. Safe to call in any mode.
func (e *Executor) WithMinReturnGate(bps int64) *Executor {
	e.minExpectedReturnBps = bps
	return e
}

// WithMaxStopLossGate attaches the CRY-37 stop-distance cap (percent
// below the signal price). Applied in route() before mode dispatch like
// the other entry gates. 0 disables.
func (e *Executor) WithMaxStopLossGate(pct float64) *Executor {
	e.maxStopLossPct = pct
	return e
}

// Mode returns the configured execution mode (exposed for /health).
func (e *Executor) GetMode() Mode { return e.mode }

// dryRun is the legacy boolean predicate kept private — every external
// caller now switches on Mode directly.
func (e *Executor) dryRun() bool { return e.mode == ModeDryRun }

// Run evaluates the market and routes signals on a fixed interval. The
// audit log is always written, regardless of routing outcome — the
// `blocked_by` column records WHY a signal did not produce a trade,
// including the new `pending_approval` value used in live mode.
func (e *Executor) Run(ctx context.Context, symbol, interval string, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	log.Printf("[executor] started — symbol=%s interval=%s every=%s mode=%s", symbol, interval, every, e.mode)

	for {
		select {
		case <-ctx.Done():
			log.Println("[executor] stopped")
			return
		case <-ticker.C:
			evalResult, err := e.analyzer.Evaluate(ctx, symbol, interval)
			if err != nil {
				log.Printf("[executor] evaluation error: %v", err)
				continue
			}
			sig := evalResult.Signal
			log.Printf("[executor] signal: %s (confidence=%.2f) — %s", sig.Action, sig.Confidence, sig.Reasoning)

			// Audit log first so we have an ID to attach to a pending
			// signal in live mode.
			auditID := e.saveAuditLog(evalResult, symbol, interval, nil, "")

			tradeID, blockedBy, err := e.route(ctx, symbol, interval, evalResult, auditID)
			if err != nil {
				log.Printf("[executor] route error: %v", err)
			}

			// Patch the audit log row with the final routing outcome.
			if tradeID != nil || blockedBy != "" {
				e.updateAuditLogOutcome(auditID, tradeID, blockedBy)
			}
		}
	}
}

// route dispatches a signal based on Mode. Returns (tradeID, blockedBy,
// err) using the same conventions as the legacy handleSignal.
//
// In ModeLive, the signal is enqueued in pending_signals and a
// `pending_approval` blockedBy reason is returned — no trade is
// created until Submit() is invoked from the approval handler.
func (e *Executor) route(
	ctx context.Context,
	symbol, interval string,
	eval *strategy.EvalResult,
	auditID uint,
) (*uint, string, error) {
	sig := eval.Signal
	if sig.Confidence < 0.6 || sig.Action == "HOLD" {
		return nil, "confidence", nil
	}

	// STR-1 / EXIT-1: reject a BUY whose AI stop-loss / take-profit sits on
	// the wrong side of the entry. The monitor exits on price ≤ StopLoss and
	// price ≥ TakeProfit, so for a long a positive SL at/above the signal
	// price (or a positive TP at/below it) fires STOP_LOSS / TAKE_PROFIT on
	// the very next tick — turning a fresh entry into a guaranteed ~2-fee-leg
	// loss. Screen both sides unconditionally and fail closed, the same way
	// the min-return gate below blocks a bad TP. A zero/absent level (the AI
	// declined to set one) is fine: snapshotSLTP stores it as nil and the
	// monitor skips it. Runs before mode dispatch so Live enqueue and
	// Paper/DryRun reject identically.
	if sig.Action == "BUY" {
		price := eval.Market.CurrentPrice
		if sig.StopLoss > 0 && sig.StopLoss >= price {
			log.Printf("[executor] BLOCKED invalid SL: sym=%s sl=%.2f >= price=%.2f (would stop out on next tick)",
				symbol, sig.StopLoss, price)
			return nil, "risk:invalid_stop_loss", nil
		}
		if sig.TakeProfit > 0 && sig.TakeProfit <= price {
			log.Printf("[executor] BLOCKED invalid TP: sym=%s tp=%.2f <= price=%.2f (would take profit on next tick)",
				symbol, sig.TakeProfit, price)
			return nil, "risk:invalid_take_profit", nil
		}
		// CRY-37: cap the loss a fresh entry accepts. Reject (never clamp)
		// a stop wider than maxStopLossPct below the signal price.
		if blocked, dist := riskmanager.MaxStopLossGate(sig.StopLoss, price, e.maxStopLossPct); blocked {
			log.Printf("[executor] BLOCKED wide SL: sym=%s sl=%.8f price=%.8f distance=%.2f%% cap=%.2f%%",
				symbol, sig.StopLoss, price, dist, e.maxStopLossPct)
			return nil, "risk:stop_loss_too_wide", nil
		}
	}

	// CRY-9 min-return gate. Applied here (before mode dispatch) so
	// Live and Paper / DryRun reject the same signals. SELL is exempt —
	// it closes an existing position, not entering a new one, so
	// expected-return-vs-fee math doesn't apply.
	if sig.Action == "BUY" && e.minExpectedReturnBps > 0 {
		if blocked, bps := e.belowMinReturn(sig.TakeProfit, eval.Market.CurrentPrice); blocked {
			log.Printf("[executor] BLOCKED by min-return gate: sym=%s expected=%d bps cap=%d bps tp=%.2f price=%.2f",
				symbol, bps, e.minExpectedReturnBps, sig.TakeProfit, eval.Market.CurrentPrice)
			return nil, "risk:below_min_return", nil
		}
	}

	if e.mode == ModeLive {
		return e.enqueueForApproval(ctx, symbol, interval, eval, auditID)
	}
	// dry-run and paper share the same auto-execute path; openTrade /
	// closeTrade themselves no-op when dryRun() is true. auditID is
	// threaded through so the close path can patch the audit row with
	// the resulting trade id.
	return e.handleSignal(ctx, symbol, eval, auditID)
}

// enqueueForApproval persists a pending_signal row and returns
// blocked_by=pending_approval (or queue_full when over capacity).
// Risk gates are NOT run at enqueue time — they run when the user
// approves, so a stale risk state during the TTL window cannot let
// through a now-disallowed order.
func (e *Executor) enqueueForApproval(
	ctx context.Context,
	symbol, interval string,
	eval *strategy.EvalResult,
	auditID uint,
) (*uint, string, error) {
	in := approval.EnqueueInput{
		Symbol:      symbol,
		Interval:    interval,
		AuditLogID:  auditID,
		Action:      eval.Signal.Action,
		Confidence:  eval.Signal.Confidence,
		Reasoning:   eval.Signal.Reasoning,
		ProposedSL:  decimal.NewFromFloat(eval.Signal.StopLoss),
		ProposedTP:  decimal.NewFromFloat(eval.Signal.TakeProfit),
		MarketPrice: decimal.NewFromFloat(eval.Market.CurrentPrice),
	}
	ps, err := e.approval.Enqueue(ctx, in)
	if err != nil {
		if errors.Is(err, approval.ErrQueueFull) {
			// Auto-reject policy: newest signal is dropped, queue
			// retains older pending rows until they expire or are
			// decided. Deterministic + documented (see PR description
			// section "Queue saturation policy"). Operator gets:
			//   - an audit_log row with blocked_by=queue_full
			//     (caller patches via updateAuditLogOutcome)
			//   - a WS event so the dashboard can flash a warning
			//   - a [executor] log line with the rejected symbol
			log.Printf("[executor] queue_full — dropped sym=%s action=%s confidence=%.2f (audit_log_id=%d)",
				symbol, eval.Signal.Action, eval.Signal.Confidence, auditID)
			if e.notifier != nil {
				e.notifier.Broadcast(map[string]any{
					"type": "signal.queue_full",
					"data": map[string]any{
						"symbol":       symbol,
						"interval":     interval,
						"action":       eval.Signal.Action,
						"confidence":   eval.Signal.Confidence,
						"audit_log_id": auditID,
						"reason":       "queue capacity exceeded — older pending signals must clear first",
					},
				})
			}
			return nil, "queue_full", nil
		}
		return nil, "approval_error", err
	}
	log.Printf("[executor] enqueued for approval id=%d sym=%s action=%s", ps.ID, ps.Symbol, ps.Action)
	if e.notifier != nil {
		e.notifier.Broadcast(map[string]any{"type": "signal.pending", "data": ps})
	}
	return nil, "pending_approval", nil
}

// Submit is called by the approval HTTP handler once a user has
// approved a pending signal. It re-fetches the underlying signal from
// the state machine (immutable input), re-runs PreTradeCheck (risk may
// have changed during TTL), and creates the Trade row.
//
// Implements handlers.Submitter. Errors here are bubbled back to the
// caller so the HTTP layer can mark the pending row as failed.
func (e *Executor) Submit(c *gin.Context, pendingID uint) (uint, error) {
	if e.mode != ModeLive {
		return 0, fmt.Errorf("executor not in live mode (got %q)", e.mode)
	}
	ctx := c.Request.Context()

	ps, err := e.approval.GetByID(ctx, pendingID)
	if err != nil {
		return 0, fmt.Errorf("fetch pending: %w", err)
	}
	// The handler transitions approved → executing BEFORE invoking
	// Submit, so the row here MUST be in `executing`. A different
	// status means concurrent submission or sweeper recovery races —
	// we refuse rather than overwrite.
	if ps.Status != models.PendingSignalExecuting {
		return 0, fmt.Errorf("pending signal status=%s (want executing)", ps.Status)
	}

	// Per-symbol duplicate-position guard. RiskManager checks the TOTAL
	// open-position count, not per-symbol uniqueness — without this
	// check, two approvals for the same symbol could open two positions
	// simultaneously. Standard, not a bandaid: structural invariant at
	// the executor boundary.
	var existing models.Position
	hasPosition := e.db.WithContext(ctx).
		Where("symbol = ? AND is_open = ?", ps.Symbol, true).
		First(&existing).Error == nil
	switch {
	case ps.Action == "BUY" && hasPosition:
		return 0, fmt.Errorf("duplicate_position: already open on %s", ps.Symbol)
	case ps.Action == "SELL" && !hasPosition:
		return 0, fmt.Errorf("no_position: cannot SELL %s — no open position", ps.Symbol)
	}

	// Re-run risk gate at submission time, never at enqueue time.
	if e.risk != nil {
		equity := e.getEquity()
		side := models.TradeSideBuy
		if ps.Action == "SELL" {
			side = models.TradeSideSell
		}
		if err := e.risk.PreTradeCheck(ps.Symbol, side, equity); err != nil {
			return 0, fmt.Errorf("risk: %w", err)
		}
	}

	if ps.Action == "SELL" {
		return e.submitClose(ctx, ps, &existing)
	}
	return e.submitOpen(ctx, ps)
}

// submitOpen creates a Trade + Position for a BUY approval. The signed
// Binance order is placed via the injected ExecutionProvider with a
// deterministic newClientOrderId="cry5-<pending_signal_id>" so a retry
// of the same approval cannot double-place. Trade row reflects the
// actual filled qty + avg fill price; entry_price uses Binance fills
// (not the signal snapshot) so the slippage check is honest.
func (e *Executor) submitOpen(ctx context.Context, ps *models.PendingSignal) (uint, error) {
	if e.provider == nil {
		return e.recordFailedTrade(ctx, ps, models.TradeSideBuy, decimal.Zero, "provider_unavailable", "execution provider not configured")
	}
	quoteAmount := decimal.Zero
	if e.risk != nil && !ps.MarketPrice.IsZero() {
		equity := e.getEquity()
		// ComputePositionSize returns base qty; convert to quote-currency
		// amount (USDT) so MARKET BUY uses quoteOrderQty — the natural
		// shape for "spend N% of equity".
		baseQty := e.risk.ComputePositionSize(equity, ps.MarketPrice)
		quoteAmount = baseQty.Mul(ps.MarketPrice)
	}
	if quoteAmount.IsZero() {
		return e.recordFailedTrade(ctx, ps, models.TradeSideBuy, decimal.Zero, "sizing_zero", "risk manager returned zero size")
	}

	clientOrderID := clientOrderIDFor(ps.ID)
	resp, err := e.provider.PlaceOrder(ctx, execution.OrderRequest{
		Symbol:        ps.Symbol,
		ClientOrderID: clientOrderID,
		Side:          execution.SideBuy,
		Type:          execution.OrderMarket,
		QuoteQuantity: quoteAmount,
	})
	if err != nil {
		return e.recordFailedTrade(ctx, ps, models.TradeSideBuy, decimal.Zero, providerFailReason(err), err.Error())
	}
	if resp.FilledQty.IsZero() {
		return e.recordFailedTrade(ctx, ps, models.TradeSideBuy, decimal.Zero,
			"exchange:"+string(resp.Status),
			fmt.Sprintf("non-filled terminal status %s", resp.Status))
	}
	warnPartialFill("BUY", resp)
	if exceeded, bps := e.slippageExceeded(ps.MarketPrice, resp.AvgFillPrice); exceeded {
		return e.recordFailedTrade(ctx, ps, models.TradeSideBuy, resp.AvgFillPrice,
			"slippage_exceeded",
			fmt.Sprintf("drift=%s bps cap=%d bps expected=%s actual=%s", bps, e.maxSlippageBps, ps.MarketPrice, resp.AvgFillPrice))
	}

	// CRY-9: real Binance commission from the per-fill array. Mixed
	// BNB / USDT receipts are rare on Spot but possible — we only
	// accept the USDT subset and log a warning otherwise. Operator
	// runbook: disable "Use BNB for fees" on testnet/live until BNB
	// → USDT conversion lands.
	// EXP-1 / EXP-2: a Spot MARKET BUY is commissioned in the BASE asset
	// received (e.g. BTC), not USDT. The old USDT-only sum therefore recorded
	// EntryFee as 0 and stored the GROSS fill as the quantity. Value the
	// commission in USDT, and net the base-asset commission out of the held
	// quantity so a later close SELL sizes against real holdings.
	base := baseAssetOf(ps.Symbol, quoteAsset)
	entryFee, feeUnvalued := feeFromFillsValued(resp.Fills, quoteAsset, base, tickerPricer(ctx, e.provider))
	if feeUnvalued {
		log.Printf("[executor] WARN: trade %s — commission in an asset that can't be valued in %s (e.g. BNB); EntryFee may understate cost",
			resp.ClientOrderID, quoteAsset)
	}
	filledQty := resp.FilledQty.Sub(baseCommissionQty(resp.Fills, base)) // base actually received
	entryCost := resp.AvgFillPrice.Mul(resp.FilledQty)                   // gross USDT deployed (cost basis denominator)
	trade := models.Trade{
		Symbol:          ps.Symbol,
		Side:            models.TradeSideBuy,
		Status:          models.TradeStatusOpen,
		EntryPrice:      resp.AvgFillPrice,
		Quantity:        filledQty,
		EntryCost:       entryCost,
		EntryFee:        entryFee,
		FeeCurrency:     quoteAsset,
		ExchangeOrderID: resp.ExchangeOrderID,
		ClientOrderID:   resp.ClientOrderID,
		AIConfidence:    ps.Confidence,
		AIReasoning:     ps.Reasoning,
		EntryAt:         time.Now(),
		IsPaper:         e.mode == ModePaper, // false in live (this path only runs on real fill)
	}
	if err := e.db.WithContext(ctx).Create(&trade).Error; err != nil {
		return 0, fmt.Errorf("create trade: %w", err)
	}
	// CRY-9 money-movement audit: every live fee charge is logged so an
	// operator running `tail -f` or `journalctl -u cryptobot` sees the
	// USDT figure leaving the wallet without grep'ing the DB. Trade row
	// is the durable record; this log line is the realtime signal.
	log.Printf("[executor] live OPEN: sym=%s cost=%s entry_fee=%s USDT held_qty=%s fills=%d trade_id=%d client=%s",
		ps.Symbol, entryCost, entryFee, filledQty, len(resp.Fills), trade.ID, resp.ClientOrderID)

	// CRY-16: snapshot SL/TP from the pending_signal (operator-approved)
	// onto the Position row. These remain immutable for the lifetime of
	// the position; subsequent AI evaluations that propose new SL/TP do
	// NOT mutate this snapshot. The monitor reads it to decide TP/SL
	// exits without consulting fresh AI output.
	sl, tp := snapshotSLTPDecimal(ps.ProposedSL, ps.ProposedTP)
	pos := models.Position{
		Symbol:        ps.Symbol,
		Side:          models.TradeSideBuy,
		Quantity:      filledQty,
		AvgEntryPrice: resp.AvgFillPrice,
		IsOpen:        true,
		TotalCost:     entryCost,
		TotalFees:     entryFee,
		StopLoss:      sl,
		TakeProfit:    tp,
	}
	if err := e.db.WithContext(ctx).Create(&pos).Error; err != nil {
		log.Printf("[executor] WARN: trade %d created but position insert failed: %v", trade.ID, err)
	}
	e.linkAuditLog(ctx, ps.AuditLogID, &trade.ID, "")
	return trade.ID, nil
}

// snapshotSLTPDecimal mirrors snapshotSLTP but takes decimal.Decimal
// (the shape PendingSignal stores). Zero values become nil — same rule
// as the float variant.
func snapshotSLTPDecimal(stopLoss, takeProfit decimal.Decimal) (sl *decimal.Decimal, tp *decimal.Decimal) {
	if stopLoss.IsPositive() {
		v := stopLoss
		sl = &v
	}
	if takeProfit.IsPositive() {
		v := takeProfit
		tp = &v
	}
	return sl, tp
}

// submitClose closes an existing Position via a SELL approval. Places
// a signed MARKET SELL for pos.Quantity via the provider, applies the
// slippage check against ps.MarketPrice, then routes the actual fill
// through closePosition with exit_reason=AI_SELL. The CAS inside
// closePosition is the structural guard against a monitor TP/SL win
// racing this approval — if the monitor has already closed the position
// while the user was approving, this returns 0,nil instead of duplicating
// the close.
func (e *Executor) submitClose(ctx context.Context, ps *models.PendingSignal, pos *models.Position) (uint, error) {
	if e.provider == nil {
		return e.recordFailedTrade(ctx, ps, models.TradeSideSell, decimal.Zero, "provider_unavailable", "execution provider not configured")
	}

	// CRY-30: one SELL per symbol at a time — a monitor exit may be
	// selling this inventory right now.
	if !e.exits.acquire(ps.Symbol) {
		return e.recordFailedTrade(ctx, ps, models.TradeSideSell, decimal.Zero, "exit_in_flight",
			"a monitor exit SELL is already in flight for "+ps.Symbol)
	}
	defer e.exits.release(ps.Symbol)
	// Re-read the position under the slot: a monitor exit that finished
	// while this approval waited may have closed or reduced it.
	var current models.Position
	if err := e.db.WithContext(ctx).
		Where("symbol = ? AND is_open = ?", ps.Symbol, true).
		First(&current).Error; err != nil {
		return e.recordFailedTrade(ctx, ps, models.TradeSideSell, decimal.Zero, "already_closed",
			"position closed by another path before SELL was placed")
	}
	pos = &current

	clientOrderID := clientOrderIDFor(ps.ID)
	resp, err := e.provider.PlaceOrder(ctx, execution.OrderRequest{
		Symbol:        ps.Symbol,
		ClientOrderID: clientOrderID,
		Side:          execution.SideSell,
		Type:          execution.OrderMarket,
		Quantity:      pos.Quantity,
	})
	if err != nil {
		return e.recordFailedTrade(ctx, ps, models.TradeSideSell, decimal.Zero, providerFailReason(err), err.Error())
	}
	if resp.FilledQty.IsZero() {
		return e.recordFailedTrade(ctx, ps, models.TradeSideSell, decimal.Zero,
			"exchange:"+string(resp.Status),
			fmt.Sprintf("non-filled terminal status %s", resp.Status))
	}
	warnPartialFill("SELL", resp)
	if exceeded, bps := e.slippageExceeded(ps.MarketPrice, resp.AvgFillPrice); exceeded {
		return e.recordFailedTrade(ctx, ps, models.TradeSideSell, resp.AvgFillPrice,
			"slippage_exceeded",
			fmt.Sprintf("drift=%s bps cap=%d bps expected=%s actual=%s", bps, e.maxSlippageBps, ps.MarketPrice, resp.AvgFillPrice))
	}

	// EXP-1: a Spot MARKET SELL is commissioned in the quote asset (USDT),
	// but value it through the same helper as the entry leg so any non-quote
	// commission is handled consistently rather than silently dropped.
	exitFee, exitFeeUnvalued := feeFromFillsValued(resp.Fills, quoteAsset, baseAssetOf(ps.Symbol, quoteAsset), tickerPricer(ctx, e.provider))
	if exitFeeUnvalued {
		log.Printf("[executor] WARN: trade %s close — commission in an asset that can't be valued in %s (e.g. BNB); ExitFee may understate cost",
			resp.ClientOrderID, quoteAsset)
	}

	tradeID, err := e.closePosition(ctx, closeFillInput{
		Symbol:          ps.Symbol,
		ExitPrice:       resp.AvgFillPrice,
		ExitQty:         resp.FilledQty,
		ExitFee:         exitFee,
		DustQty:         minQtyFor(ctx, e.provider, ps.Symbol),
		ExitReason:      models.ExitReasonAISell,
		AIConfidence:    ps.Confidence,
		AuditLogID:      ps.AuditLogID,
		ExchangeOrderID: resp.ExchangeOrderID,
		ClientOrderID:   resp.ClientOrderID,
		IsPaper:         false,
	})
	if errors.Is(err, ErrAlreadyClosed) {
		// Monitor (or another concurrent SELL) closed it first — the
		// venue has executed a SELL on top of the previously-closed
		// inventory. This is a real money-movement event for the
		// reconciler to investigate; we surface it as a failed trade
		// row so the audit trail captures the orphaned fill.
		log.Printf("[executor] live SELL fill orphaned by prior close: sym=%s qty=%s price=%s client=%s",
			ps.Symbol, resp.FilledQty, resp.AvgFillPrice, resp.ClientOrderID)
		return e.recordFailedTrade(ctx, ps, models.TradeSideSell, resp.AvgFillPrice,
			"already_closed",
			fmt.Sprintf("position closed by another path before SELL fill; orphan qty=%s", resp.FilledQty))
	}
	return tradeID, err
}

// clientOrderIDFor is the canonical idempotency key for a pending signal.
// Persisted to Binance via newClientOrderId; a retry of the same approval
// MUST send the same id so Binance dedupes server-side.
func clientOrderIDFor(pendingID uint) string {
	return fmt.Sprintf("cry5-%d", pendingID)
}

// closeFillInput bundles everything closePosition needs to settle a
// CLOSED row + roll position aggregates. The caller decides the exit
// price + qty + fee + reason; closePosition does the CAS, the math,
// and the audit linkage uniformly.
type closeFillInput struct {
	Symbol     string
	ExitPrice  decimal.Decimal
	ExitQty    decimal.Decimal
	ExitFee    decimal.Decimal
	ExitReason models.ExitReason

	// DustQty is the largest remainder still treated as a FULL close
	// (the venue's LOT_SIZE minQty — a remainder at or below it can't
	// be sold anyway). Zero means any positive remainder keeps the
	// position open with the reduced qty.
	DustQty decimal.Decimal

	// AI provenance — empty for monitor-triggered closes that bring
	// their own audit row.
	AIConfidence float64

	// AuditLogID links the close to an audit_log row. Required: every
	// close MUST be traceable to either an AI evaluation (AI_SELL) or
	// a synthetic monitor audit row (TAKE_PROFIT / STOP_LOSS).
	AuditLogID uint

	// Exchange ids — set by live submitClose + reconciler, zero for paper.
	ExchangeOrderID int64
	ClientOrderID   string

	IsPaper bool
}

// closePosition is the SINGLE close-path entry. Every close trigger
// (AI_SELL, TAKE_PROFIT, STOP_LOSS, TRAILING_STOP, MANUAL_CLOSE,
// RECONCILE_CLOSE) converges here. settleClose does the CAS + PnL math
// in one DB transaction; concurrent losers get ErrAlreadyClosed and
// MUST treat that as a no-op (the end state they wanted is already
// true).
//
// Standard, not bandaid: the runtime cannot grow a new close path that
// bypasses this function. A new exit trigger registers a new
// models.ExitReason value and calls closePosition.
func (e *Executor) closePosition(ctx context.Context, in closeFillInput) (uint, error) {
	res, err := settleClose(ctx, e.db, in)
	if err != nil {
		return 0, err
	}

	if in.AuditLogID != 0 {
		e.linkAuditLog(ctx, in.AuditLogID, &res.TradeID, "")
	}

	if e.risk != nil {
		e.risk.PostTradeCheck(e.getEquity())
	}

	label := "live"
	if in.IsPaper {
		label = "paper"
	}
	log.Printf("[executor] %s CLOSE: sym=%s qty=%s remaining=%s exit_cost=%s exit_fee=%s pnl=%s net_pnl=%s reason=%s trade_id=%d",
		label, in.Symbol, in.ExitQty, res.RemainingQty, res.ExitCost, in.ExitFee, res.PnL, res.NetPnL, in.ExitReason, res.TradeID)
	return res.TradeID, nil
}

// closeResult is what settleClose booked.
type closeResult struct {
	TradeID      uint
	ExitCost     decimal.Decimal
	PnL          decimal.Decimal
	NetPnL       decimal.Decimal
	RemainingQty decimal.Decimal // > 0 only on a partial close
}

// settleClose books an exit fill against the open position in a single
// DB transaction, so a crash can never leave a closed position with an
// OPEN trade (or the reverse). Shared by the executor and the
// reconciler — one copy of the PnL math.
//
// Full close (remainder ≤ DustQty): CAS flips is_open=false; the OPEN
// trade row becomes CLOSED.
//
// Partial close (remainder > DustQty — LOT_SIZE floor on the SELL, or a
// venue partial fill): CAS reduces positions.quantity; the sold part is
// split off into its own CLOSED trade carrying a pro-rata share of the
// entry cost + entry fee, and the OPEN trade keeps the rest. The
// position stays open so the monitor / AI can exit the remainder.
//
// NetPnL deducts both fee legs so equity sees what the wallet pockets.
func settleClose(ctx context.Context, db *gorm.DB, in closeFillInput) (closeResult, error) {
	var res closeResult
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var pos models.Position
		if err := tx.Where("symbol = ? AND is_open = ?", in.Symbol, true).
			Order("id DESC").First(&pos).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrAlreadyClosed
			}
			return fmt.Errorf("load open position: %w", err)
		}
		var trade models.Trade
		if err := tx.Where("symbol = ? AND status = ?", in.Symbol, models.TradeStatusOpen).
			Order("id DESC").First(&trade).Error; err != nil {
			return fmt.Errorf("find open trade: %w", err)
		}

		remaining := pos.Quantity.Sub(in.ExitQty)
		partial := in.ExitQty.IsPositive() && remaining.GreaterThan(in.DustQty)

		// ── CAS gate ─────────────────────────────────────────────
		// Exactly one concurrent caller matches the WHERE; the loser
		// sees RowsAffected=0 and bails without touching anything.
		cas := tx.Model(&models.Position{}).Where("id = ? AND is_open = ?", pos.ID, true)
		if partial {
			cas = cas.Where("quantity = ?", pos.Quantity).Update("quantity", remaining)
		} else {
			cas = cas.Update("is_open", false)
		}
		if cas.Error != nil {
			return fmt.Errorf("close cas: %w", cas.Error)
		}
		if cas.RowsAffected == 0 {
			return ErrAlreadyClosed
		}

		// Entry cost / fee attributable to the qty that exited.
		entryCost, entryFee := trade.EntryCost, trade.EntryFee
		if partial && pos.Quantity.IsPositive() {
			ratio := in.ExitQty.Div(pos.Quantity)
			entryCost = trade.EntryCost.Mul(ratio).Round(8)
			entryFee = trade.EntryFee.Mul(ratio).Round(8)
		}

		exitCost := in.ExitPrice.Mul(in.ExitQty)
		pnl := in.ExitPrice.Sub(pos.AvgEntryPrice).Mul(in.ExitQty)
		pnlPct := decimal.Zero
		if !pos.AvgEntryPrice.IsZero() {
			pnlPct = in.ExitPrice.Sub(pos.AvgEntryPrice).Div(pos.AvgEntryPrice).Mul(decimal.NewFromInt(100))
		}
		netPnL := pnl.Sub(entryFee).Sub(in.ExitFee)
		netPnLPct := pnlPct
		if !entryCost.IsZero() {
			netPnLPct = netPnL.Div(entryCost).Mul(decimal.NewFromInt(100))
		}
		now := time.Now()

		exitFields := map[string]any{
			"status":          models.TradeStatusClosed,
			"exit_price":      in.ExitPrice,
			"quantity":        in.ExitQty,
			"exit_cost":       exitCost,
			"exit_fee":        in.ExitFee,
			"pnl":             pnl,
			"pnl_percent":     pnlPct,
			"net_pnl":         netPnL,
			"net_pnl_percent": netPnLPct,
			"exit_at":         &now,
			"exit_reason":     in.ExitReason,
		}
		if in.ExchangeOrderID != 0 {
			exitFields["exchange_order_id"] = in.ExchangeOrderID
		}
		if in.ClientOrderID != "" {
			exitFields["client_order_id"] = in.ClientOrderID
		}

		closedID := trade.ID
		if partial {
			// Split the sold part off into its own CLOSED row; the
			// OPEN row keeps the remainder's cost basis.
			part := trade
			part.ID = 0
			part.EntryCost = entryCost
			part.EntryFee = entryFee
			if err := tx.Create(&part).Error; err != nil {
				return fmt.Errorf("create partial close trade: %w", err)
			}
			closedID = part.ID
			if err := tx.Model(&models.Trade{}).Where("id = ?", trade.ID).Updates(map[string]any{
				"quantity":   remaining,
				"entry_cost": trade.EntryCost.Sub(entryCost),
				"entry_fee":  trade.EntryFee.Sub(entryFee),
			}).Error; err != nil {
				return fmt.Errorf("reduce open trade: %w", err)
			}
		}
		if err := tx.Model(&models.Trade{}).Where("id = ?", closedID).Updates(exitFields).Error; err != nil {
			return fmt.Errorf("close trade: %w", err)
		}

		// Roll per-symbol lifetime aggregates (CRY-6: RealizedPnL
		// accumulates NET).
		if err := tx.Model(&models.Position{}).Where("id = ?", pos.ID).Updates(map[string]any{
			"realized_pnl": gorm.Expr("realized_pnl + ?", netPnL),
			"total_fees":   gorm.Expr("total_fees + ?", in.ExitFee),
		}).Error; err != nil {
			return fmt.Errorf("roll position aggregates: %w", err)
		}

		if !partial && remaining.IsPositive() {
			log.Printf("[executor] %s close left dust qty=%s (≤ minQty %s) — not sellable, position closed",
				in.Symbol, remaining, in.DustQty)
		}
		res = closeResult{TradeID: closedID, ExitCost: exitCost, PnL: pnl, NetPnL: netPnL}
		if partial {
			res.RemainingQty = remaining
		}
		return nil
	})
	return res, err
}

// providerFailReason maps execution sentinel errors to short stable
// strings suitable for trades.error and the WS reason field. Unknown
// errors collapse to "exchange_error" — the full message is in
// trades.error for forensics.
func providerFailReason(err error) string {
	switch {
	case errors.Is(err, execution.ErrLotSizeTooSmall):
		return "lot_size_too_small"
	case errors.Is(err, execution.ErrNotionalTooSmall):
		return "notional_too_small"
	case errors.Is(err, execution.ErrInsufficientBalance):
		return "insufficient_balance"
	case errors.Is(err, execution.ErrSignatureRejected):
		return "signature_rejected"
	case errors.Is(err, execution.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, execution.ErrOrderNotFound):
		return "order_not_found"
	case errors.Is(err, execution.ErrPollTimeout):
		return "poll_timeout"
	case errors.Is(err, execution.ErrAmbiguousPlacement):
		return "ambiguous_placement"
	case errors.Is(err, execution.ErrSymbolNotFound):
		return "symbol_not_found"
	}
	return "exchange_error"
}

// belowMinReturn delegates to the shared riskmanager.MinExpectedReturnGate.
// CRY-10 extracted the formula so the backtester applies the identical gate.
func (e *Executor) belowMinReturn(takeProfit, currentPrice float64) (bool, int64) {
	return riskmanager.MinExpectedReturnGate(takeProfit, currentPrice, e.minExpectedReturnBps)
}

// quoteAsset is the settlement currency for every pair this bot trades.
// All pricing, sizing, and PnL are denominated in it. Kept as a single
// const so the fee-valuation helpers and FeeCurrency stay consistent.
const quoteAsset = "USDT"

// feeFromFillsValued totals Binance commission across fills, expressed in the
// quote asset (USDT). EXP-1: a Spot MARKET BUY is commissioned in the BASE
// asset received (e.g. BTC), so the old USDT-only sum recorded the entry fee
// as 0. Here a quote-asset commission is taken as-is; a base-asset commission
// is valued at that fill's price; any other asset (e.g. BNB when "Use BNB for
// fees" is enabled) is valued through price (CRY-35). When price is nil or
// cannot value the asset, the commission is skipped and unvalued is returned
// true so the caller can alert.
func feeFromFillsValued(fills []execution.Fill, quote, base string, price assetPricer) (fee decimal.Decimal, unvalued bool) {
	fee = decimal.Zero
	for _, f := range fills {
		switch f.CommissionAsset {
		case quote:
			fee = fee.Add(f.Commission)
		case base:
			fee = fee.Add(f.Commission.Mul(f.Price)) // value base commission in quote at the fill price
		default:
			if !f.Commission.IsPositive() {
				continue
			}
			if price != nil {
				if p, ok := price(f.CommissionAsset); ok {
					fee = fee.Add(f.Commission.Mul(p))
					continue
				}
			}
			unvalued = true
		}
	}
	return fee, unvalued
}

// tickerLookupTimeout bounds one commission-pricing lookup at settle.
const tickerLookupTimeout = 5 * time.Second

// assetPricer values one unit of asset in the quote asset. ok=false when
// no price is available.
type assetPricer func(asset string) (decimal.Decimal, bool)

// tickerPricer prices a third-asset commission (BNB) from the venue's
// public ticker <asset><quote>. Memoized per returned func — callers build
// one per order, so each asset is looked up at most once per settle. A
// lookup failure is logged and leaves the commission unvalued; it never
// blocks the settle.
func tickerPricer(ctx context.Context, p execution.ExecutionProvider) assetPricer {
	if p == nil {
		return nil
	}
	type result struct {
		price decimal.Decimal
		ok    bool
	}
	memo := map[string]result{}
	return func(asset string) (decimal.Decimal, bool) {
		if r, seen := memo[asset]; seen {
			return r.price, r.ok
		}
		// The order already filled — don't hold the settle on a slow ticker.
		lookupCtx, cancel := context.WithTimeout(ctx, tickerLookupTimeout)
		price, err := p.GetTickerPrice(lookupCtx, asset+quoteAsset)
		cancel()
		if err != nil {
			log.Printf("[executor] WARN: cannot price %s commission in %s: %v", asset, quoteAsset, err)
		}
		r := result{price: price, ok: err == nil && price.IsPositive()}
		memo[asset] = r
		return r.price, r.ok
	}
}

// baseCommissionQty totals the commission charged in the base asset — the
// quantity the wallet did NOT actually receive on a Spot MARKET BUY. EXP-2:
// the position quantity must net this out so a later close SELL sizes against
// real holdings instead of being rejected for insufficient balance.
func baseCommissionQty(fills []execution.Fill, base string) decimal.Decimal {
	total := decimal.Zero
	if base == "" {
		return total
	}
	for _, f := range fills {
		if f.CommissionAsset == base {
			total = total.Add(f.Commission)
		}
	}
	return total
}

// warnPartialFill logs a terminal order that filled only part of its
// size (EXPIRED / CANCELED with executedQty > 0). The filled part is
// real money movement and is settled like any fill.
func warnPartialFill(side string, resp execution.OrderResponse) {
	if resp.Status != execution.StatusFilled {
		log.Printf("[executor] WARN: %s %s ended %s with partial fill qty=%s — settling the filled part",
			side, resp.ClientOrderID, resp.Status, resp.FilledQty)
	}
}

// minQtyFor returns the symbol's LOT_SIZE minQty, used as the dust
// threshold on close. Zero on lookup failure — the close then keeps any
// remainder open rather than silently dropping it.
func minQtyFor(ctx context.Context, p execution.ExecutionProvider, symbol string) decimal.Decimal {
	if p == nil {
		return decimal.Zero
	}
	f, err := p.GetSymbolFilters(ctx, symbol)
	if err != nil {
		log.Printf("[executor] WARN: symbol filters for %s unavailable (%v) — no dust threshold", symbol, err)
		return decimal.Zero
	}
	return f.MinQty
}

// baseAssetOf strips the quote suffix to recover the base asset, e.g.
// ("BTCUSDT","USDT") -> "BTC". Returns "" when symbol does not end in quote
// (defensive: callers treat "" as "no base-asset commission possible").
func baseAssetOf(symbol, quote string) string {
	if n := len(symbol) - len(quote); n > 0 && symbol[n:] == quote {
		return symbol[:n]
	}
	return ""
}

// slippageExceeded compares the qty-weighted fill price against the
// signal-time market price. Returns the drift in basis points alongside
// the exceeded flag so the caller can log it. A zero cap disables the
// check (test convenience; production always sets a positive cap).
func (e *Executor) slippageExceeded(expected, actual decimal.Decimal) (bool, decimal.Decimal) {
	if e.maxSlippageBps <= 0 || expected.IsZero() || actual.IsZero() {
		return false, decimal.Zero
	}
	diff := actual.Sub(expected).Abs()
	bps := diff.Div(expected).Mul(decimal.NewFromInt(10000))
	return bps.GreaterThan(decimal.NewFromInt(e.maxSlippageBps)), bps
}

// recordFailedTrade writes a Trade{Status=FAILED, Error=<detail>} row,
// broadcasts a signal.failed WS event, and returns an error that bubbles
// up to the approval handler so the state machine transitions to FAILED.
// Standard, not a bandaid: every failure path produces a durable trade
// row tagged with a short stable reason — the operator never has to
// grep logs to learn why an approved signal did not execute.
func (e *Executor) recordFailedTrade(
	ctx context.Context,
	ps *models.PendingSignal,
	side models.TradeSide,
	fillPrice decimal.Decimal,
	reason, detail string,
) (uint, error) {
	entryPrice := fillPrice
	if entryPrice.IsZero() {
		entryPrice = ps.MarketPrice
	}
	trade := models.Trade{
		Symbol:        ps.Symbol,
		Side:          side,
		Status:        models.TradeStatusFailed,
		EntryPrice:    entryPrice,
		Quantity:      decimal.Zero,
		AIConfidence:  ps.Confidence,
		AIReasoning:   ps.Reasoning,
		ClientOrderID: clientOrderIDFor(ps.ID),
		Error:         fmt.Sprintf("%s: %s", reason, detail),
		EntryAt:       time.Now(),
		IsPaper:       e.mode == ModePaper, // FAILED rows only originate from live-mode execution attempts; flag still matches runtime mode
	}
	if err := e.db.WithContext(ctx).Create(&trade).Error; err != nil {
		log.Printf("[executor] WARN: could not persist FAILED trade for ps=%d: %v", ps.ID, err)
	}
	e.linkAuditLog(ctx, ps.AuditLogID, &trade.ID, "execution_failed:"+reason)

	if e.notifier != nil {
		e.notifier.Broadcast(map[string]any{
			"type": "signal.failed",
			"data": map[string]any{
				"pending_signal_id": ps.ID,
				"symbol":            ps.Symbol,
				"action":            ps.Action,
				"reason":            reason,
				"detail":            detail,
				"trade_id":          trade.ID,
			},
		})
	}
	return trade.ID, fmt.Errorf("%s: %s", reason, detail)
}

// linkAuditLog patches the existing audit_logs row (created at signal-
// route time) with the trade_id and an optional executed/blocked status.
// Best-effort: a missing audit_log_id (legacy data) is a silent no-op.
func (e *Executor) linkAuditLog(ctx context.Context, auditID uint, tradeID *uint, blockedBy string) {
	if auditID == 0 {
		return
	}
	updates := map[string]any{}
	if tradeID != nil {
		updates["trade_id"] = *tradeID
		if blockedBy == "" {
			updates["executed"] = true
		}
	}
	if blockedBy != "" {
		updates["blocked_by"] = blockedBy
	}
	if len(updates) == 0 {
		return
	}
	if err := e.db.WithContext(ctx).Model(&models.AuditLog{}).
		Where("id = ?", auditID).Updates(updates).Error; err != nil {
		log.Printf("[executor] failed to link audit_log id=%d: %v", auditID, err)
	}
}

// handleSignal returns (tradeID or nil, blockedBy reason, error). Paper
// and dry-run path — Live routes through enqueueForApproval instead.
func (e *Executor) handleSignal(ctx context.Context, symbol string, eval *strategy.EvalResult, auditID uint) (*uint, string, error) {
	signal := eval.Signal
	if signal.Confidence < 0.6 || signal.Action == "HOLD" {
		return nil, "confidence", nil
	}

	// Check for existing open position
	var pos models.Position
	hasPosition := e.db.Where("symbol = ? AND is_open = ?", symbol, true).First(&pos).Error == nil

	switch signal.Action {
	case "BUY":
		if hasPosition {
			log.Println("[executor] already in position, skipping BUY")
			return nil, "duplicate_position", nil
		}

		// ── Risk gate ────────────────────────────────
		if e.risk != nil {
			equity := e.getEquity()
			if err := e.risk.PreTradeCheck(symbol, models.TradeSideBuy, equity); err != nil {
				log.Printf("[executor] BLOCKED by risk manager: %v", err)
				if breach, ok := err.(riskmanager.Breach); ok {
					return nil, "risk:" + breach.Rule, nil
				}
				return nil, "risk:unknown", nil
			}
		}

		tradeID, err := e.openTrade(ctx, symbol, eval)
		return tradeID, "", err

	case "SELL":
		if !hasPosition {
			log.Println("[executor] no open position, skipping SELL")
			return nil, "no_position", nil
		}
		tradeID, err := e.closeTrade(ctx, symbol, eval, auditID)
		return tradeID, "", err
	}

	return nil, "", nil
}

// getEquity returns total equity (base capital + net realized PnL).
// Delegates to the risk manager so sizing, the drawdown breaker and the
// risk status endpoint share one formula. Every caller is already
// guarded by e.risk != nil.
func (e *Executor) getEquity() decimal.Decimal {
	return e.risk.Equity()
}

func (e *Executor) openTrade(_ context.Context, symbol string, eval *strategy.EvalResult) (*uint, error) {
	signal := eval.Signal
	if e.dryRun() {
		log.Printf("[executor][DRY-RUN] would BUY %s at confidence %.2f", symbol, signal.Confidence)
		return nil, nil
	}

	// Paper BUY books the entry at the analyzer's last-candle close —
	// the same number the AI saw when it issued the signal. Mirrors
	// closeTrade's exitPrice sourcing so PnL math is symmetric. A zero
	// here means the analyzer didn't populate market price, which is a
	// structural bug upstream, not something to paper over with SL.
	price := decimal.NewFromFloat(eval.Market.CurrentPrice)
	if price.IsZero() {
		return nil, fmt.Errorf("paper BUY: zero market price for %s", symbol)
	}

	// Use risk manager for position sizing when available, fall back to fixed qty
	var qty decimal.Decimal
	if e.risk != nil {
		equity := e.getEquity()
		qty = e.risk.ComputePositionSize(equity, price)
		log.Printf("[executor] risk-sized position: %s %s @ %s (equity: %s)", qty, symbol, price, equity)
	} else {
		qty = decimal.NewFromFloat(0.001) // legacy fallback
	}

	// CRY-9: simulate Binance Spot taker fee (feeRate × notional) so
	// paper / dry-run PnL reflects what live would actually book. Keeps
	// backtests honest about fee impact instead of waving 0 around.
	entryCost := price.Mul(qty)
	entryFee := entryCost.Mul(e.feeRate)
	trade := models.Trade{
		Symbol:       symbol,
		Side:         models.TradeSideBuy,
		Status:       models.TradeStatusOpen,
		EntryPrice:   price,
		Quantity:     qty,
		EntryCost:    entryCost,
		EntryFee:     entryFee,
		FeeCurrency:  "USDT",
		AIConfidence: signal.Confidence,
		AIReasoning:  signal.Reasoning,
		EntryAt:      time.Now(),
		IsPaper:      true, // openTrade is reached only in ModePaper (ModeDryRun short-circuits above, ModeLive routes via approval+provider)
	}
	if err := e.db.Create(&trade).Error; err != nil {
		return nil, fmt.Errorf("save trade: %w", err)
	}
	// CRY-9 paper money-movement log — same shape as the live equivalent
	// so log consumers can grep one prefix across modes.
	log.Printf("[executor] paper OPEN: sym=%s cost=%s sim_fee=%s USDT trade_id=%d",
		symbol, entryCost, entryFee, trade.ID)

	// CRY-16: snapshot SL/TP from the AI signal as an immutable risk plan
	// on the Position row. The monitor reads these without ever consulting
	// fresh AI output — that's the structural guarantee against drift
	// between "what we said we'd exit at" and "what we actually do".
	sl, tp := snapshotSLTP(signal.StopLoss, signal.TakeProfit)
	pos := models.Position{
		Symbol:        symbol,
		Side:          models.TradeSideBuy,
		Quantity:      qty,
		AvgEntryPrice: price,
		IsOpen:        true,
		TotalCost:     entryCost,
		TotalFees:     entryFee,
		StopLoss:      sl,
		TakeProfit:    tp,
	}
	if err := e.db.Create(&pos).Error; err != nil {
		return nil, err
	}
	return &trade.ID, nil
}

// snapshotSLTP normalises raw signal floats into the *decimal.Decimal
// shape positions.stop_loss / take_profit expect. A zero or negative
// value means "the AI declined to provide" and is stored as nil so the
// monitor skips the position cleanly.
func snapshotSLTP(stopLoss, takeProfit float64) (sl *decimal.Decimal, tp *decimal.Decimal) {
	if stopLoss > 0 {
		v := decimal.NewFromFloat(stopLoss)
		sl = &v
	}
	if takeProfit > 0 {
		v := decimal.NewFromFloat(takeProfit)
		tp = &v
	}
	return sl, tp
}

// closeTrade is the paper-mode AI SELL path. The signal triggered a
// SELL with confidence ≥ 0.6; we route through closePosition so the
// CAS race against the position monitor cannot produce a double-close.
// Exit price uses the freshest signal-time market price (the analyzer's
// last candle close) rather than the AI's recommended TakeProfit —
// paper PnL stays honest about where the trade actually exited.
func (e *Executor) closeTrade(ctx context.Context, symbol string, eval *strategy.EvalResult, auditID uint) (*uint, error) {
	signal := eval.Signal
	if e.dryRun() {
		log.Printf("[executor][DRY-RUN] would SELL %s at confidence %.2f", symbol, signal.Confidence)
		return nil, nil
	}

	// Load qty + entry to compute the paper exit fee against the actual
	// position notional. closePosition reloads these too, but we need
	// the qty here to size the simulated fee BEFORE handing off.
	var pos models.Position
	if err := e.db.WithContext(ctx).
		Where("symbol = ? AND is_open = ?", symbol, true).
		First(&pos).Error; err != nil {
		return nil, fmt.Errorf("load position for paper SELL: %w", err)
	}

	exitPrice := decimal.NewFromFloat(eval.Market.CurrentPrice)
	if exitPrice.IsZero() {
		// Defensive: a missing market price (analyzer should always
		// populate this) is preferable to silently exiting at 0 and
		// producing a catastrophic PnL row.
		return nil, fmt.Errorf("paper SELL: zero market price for %s", symbol)
	}
	exitCost := exitPrice.Mul(pos.Quantity)
	exitFee := exitCost.Mul(e.feeRate)

	tradeID, err := e.closePosition(ctx, closeFillInput{
		Symbol:       symbol,
		ExitPrice:    exitPrice,
		ExitQty:      pos.Quantity,
		ExitFee:      exitFee,
		ExitReason:   models.ExitReasonAISell,
		AIConfidence: signal.Confidence,
		AuditLogID:   auditID,
		IsPaper:      true,
	})
	if errors.Is(err, ErrAlreadyClosed) {
		log.Printf("[executor] paper SELL skipped — monitor already closed %s", symbol)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tradeID, nil
}

// saveAuditLog persists the full evidence trail for every AI evaluation
// and returns the newly-created row's ID. The ID is required at call
// time when EXECUTION_MODE=live, so the pending_signals row can link
// back via audit_log_id.
//
// In live mode this is called BEFORE routing (with tradeID=nil and
// blockedBy=""), and updateAuditLogOutcome later patches the result.
// In dry-run/paper modes the values are known up front and patching is
// a no-op for tradeID=nil + blockedBy="".
func (e *Executor) saveAuditLog(eval *strategy.EvalResult, symbol, interval string, tradeID *uint, blockedBy string) uint {
	executed := tradeID != nil

	entry := models.AuditLog{
		TradeID:         tradeID,
		Symbol:          symbol,
		Interval:        interval,
		Prompt:          eval.Prompt,
		MarketPrice:     decimal.NewFromFloat(eval.Market.CurrentPrice),
		MarketRSI:       eval.Market.RSI,
		MarketMACD:      eval.Market.MACDLine,
		MarketSignal:    eval.Market.SignalLine,
		MarketVolume:    eval.Market.Volume24h,
		RawResponse:     eval.RawResponse,
		Action:          eval.Signal.Action,
		Confidence:      eval.Signal.Confidence,
		Reasoning:       eval.Signal.Reasoning,
		Executed:        executed,
		BlockedBy:       blockedBy,
		PromptSentAt:    eval.SentAt,
		ResponseAt:      eval.ReceivedAt,
		InferenceTimeMs: eval.ReceivedAt.Sub(eval.SentAt).Milliseconds(),
	}

	// CRY-22: capture queryable position context whenever the AI tick
	// had an open position on this symbol. All five columns populate
	// once PR-3 lands PositionContext.PeakPrice / DrawdownFromPeakPct;
	// PR-2 populated only the three non-peak fields, and rows from that
	// PR's window stay NULL on peak/drawdown — forensic queries should
	// filter `position_peak_price IS NOT NULL` to scope to PR-3+ data.
	if pos := eval.Market.Position; pos != nil {
		entryPrice := decimal.NewFromFloat(pos.EntryPrice)
		entry.PositionEntryPrice = &entryPrice
		unrealized := pos.UnrealizedPnLPct
		entry.PositionUnrealizedPnLPct = &unrealized
		held := pos.HoursHeld
		entry.PositionHoursHeld = &held
		if pos.PeakPrice > 0 {
			peak := decimal.NewFromFloat(pos.PeakPrice)
			entry.PositionPeakPrice = &peak
			drawdown := pos.DrawdownFromPeakPct
			entry.PositionDrawdownFromPeakPct = &drawdown
		}
	}

	if err := e.db.Create(&entry).Error; err != nil {
		log.Printf("[executor] failed to save audit log: %v", err)
		return 0
	}
	return entry.ID
}

// updateAuditLogOutcome patches the audit_log row with the final routing
// result. Called after route() returns so the audit trail reflects the
// trade ID (if any) and any blocked-by reason (confidence/risk/queue
// full/pending approval). Idempotent on (executed, blocked_by).
func (e *Executor) updateAuditLogOutcome(auditID uint, tradeID *uint, blockedBy string) {
	if auditID == 0 {
		return
	}
	updates := map[string]any{
		"blocked_by": blockedBy,
	}
	if tradeID != nil {
		updates["trade_id"] = *tradeID
		updates["executed"] = true
	}
	if err := e.db.Model(&models.AuditLog{}).Where("id = ?", auditID).Updates(updates).Error; err != nil {
		log.Printf("[executor] failed to patch audit log id=%d: %v", auditID, err)
	}
}
