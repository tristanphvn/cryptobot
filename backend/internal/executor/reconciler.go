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

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// ExchangeReconciler resolves stuck `executing` pending_signals by
// querying the exchange with newClientOrderId="cry5-<ps.ID>". Plugs into
// approval.Sweeper via WithReconciler.
//
// Standard, not a bandaid: Binance is the source of truth across a
// crash. A FILLED order at the venue becomes `submitted` locally with a
// real trade_id; a CANCELED / EXPIRED / REJECTED becomes `failed` with
// a precise reason; ErrOrderNotFound (Binance never received it) means
// the original Submit crashed before reaching the wire — the row is
// failed with reason="not_placed". Any other error (network / 5xx) is
// surfaced so the sweeper leaves the row in executing for the next tick.
type ExchangeReconciler struct {
	db             *gorm.DB
	sm             *approval.StateMachine
	provider       execution.ExecutionProvider
	notifier       Notifier
	maxSlippageBps int64
}

func NewExchangeReconciler(db *gorm.DB, sm *approval.StateMachine, provider execution.ExecutionProvider, notifier Notifier, maxSlippageBps int64) *ExchangeReconciler {
	return &ExchangeReconciler{db: db, sm: sm, provider: provider, notifier: notifier, maxSlippageBps: maxSlippageBps}
}

// Reconcile implements approval.Reconciler.
func (r *ExchangeReconciler) Reconcile(ctx context.Context, ps models.PendingSignal) error {
	clientOrderID := clientOrderIDFor(ps.ID)
	resp, err := r.provider.GetOrder(ctx, ps.Symbol, clientOrderID)
	if errors.Is(err, execution.ErrOrderNotFound) {
		return r.failWithoutTrade(ctx, ps, "not_placed", "binance has no record of "+clientOrderID)
	}
	if err != nil {
		// Ambiguous (network / rate-limit / 5xx). Surface to sweeper so
		// the row stays in executing for the next tick.
		return fmt.Errorf("getorder: %w", err)
	}

	switch resp.Status {
	case execution.StatusFilled:
		return r.recordRecoveredFill(ctx, ps, resp)
	case execution.StatusCanceled, execution.StatusExpired, execution.StatusRejected:
		// CRY-31: a terminal order can still carry a partial fill
		// (MARKET order EXPIRED on thin liquidity). That fill is real
		// money movement — record it instead of failing the row.
		if resp.FilledQty.IsPositive() {
			return r.recordRecoveredFill(ctx, ps, resp)
		}
		return r.failWithoutTrade(ctx, ps, "exchange:"+string(resp.Status), "binance terminal status "+string(resp.Status))
	default:
		// NEW / PARTIALLY_FILLED — venue says the order is still alive.
		// Return an error so the sweeper leaves it in executing.
		return fmt.Errorf("non-terminal status %s — leaving for next tick", resp.Status)
	}
}

// recordRecoveredFill creates a Trade row (and a Position on BUY, or
// closes the open Position on SELL) from the recovered fill, then
// transitions the pending_signal to submitted. Slippage is checked
// against the signal-time snapshot just like the live Submit path.
func (r *ExchangeReconciler) recordRecoveredFill(ctx context.Context, ps models.PendingSignal, resp execution.OrderResponse) error {
	if resp.FilledQty.IsZero() {
		return r.failWithoutTrade(ctx, ps, "exchange:FILLED_ZERO_QTY", "FILLED status but executedQty=0")
	}
	if exceeded, bps := r.checkSlippage(ps.MarketPrice, resp.AvgFillPrice); exceeded {
		detail := fmt.Sprintf("drift=%s bps cap=%d bps expected=%s actual=%s",
			bps, r.maxSlippageBps, ps.MarketPrice, resp.AvgFillPrice)
		return r.recordFailedRecovery(ctx, ps, resp, "slippage_exceeded", detail)
	}

	side := models.TradeSideBuy
	if ps.Action == "SELL" {
		side = models.TradeSideSell
	}

	if side == models.TradeSideBuy {
		return r.recoverOpen(ctx, ps, resp)
	}
	return r.recoverClose(ctx, ps, resp)
}

func (r *ExchangeReconciler) recoverOpen(ctx context.Context, ps models.PendingSignal, resp execution.OrderResponse) error {
	// CRY-9: same fee + cost accounting as submitOpen — recovered
	// trades must not silently drop fee data just because they came
	// in via the sweeper instead of the happy path. Operator must
	// not have to grep "is this trade missing fees because of crash
	// recovery?" — the schema looks the same regardless of origin.
	// EXP-1/EXP-2: mirror submitOpen — a Spot MARKET BUY is commissioned in
	// the base asset, so value it in USDT and net it out of the held quantity.
	base := baseAssetOf(ps.Symbol, quoteAsset)
	entryFee, feeUnvalued := feeFromFillsValued(resp.Fills, quoteAsset, base, tickerPricer(ctx, r.provider))
	if feeUnvalued {
		log.Printf("[reconciler] WARN: recovered trade %s — commission in an asset that can't be valued in %s (e.g. BNB); EntryFee may understate cost", resp.ClientOrderID, quoteAsset)
	}
	filledQty := resp.FilledQty.Sub(baseCommissionQty(resp.Fills, base))
	entryCost := resp.AvgFillPrice.Mul(resp.FilledQty)
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
		IsPaper:         false, // reconciler only runs in ModeLive (CRY-15 wires it behind `if mode == ModeLive`)
	}
	if err := r.db.WithContext(ctx).Create(&trade).Error; err != nil {
		return fmt.Errorf("create recovered trade: %w", err)
	}
	log.Printf("[reconciler] recovered OPEN: sym=%s cost=%s entry_fee=%s USDT fills=%d trade_id=%d client=%s",
		ps.Symbol, entryCost, entryFee, len(resp.Fills), trade.ID, resp.ClientOrderID)
	if err := r.sm.MarkSubmitted(ctx, ps.ID, trade.ID); err != nil {
		log.Printf("[reconciler] id=%d MarkSubmitted err: %v (trade %d already created)", ps.ID, err, trade.ID)
		return nil
	}
	// CRY-31: carry the operator-approved SL/TP onto the recovered
	// position, same as submitOpen — otherwise a crash-recovered
	// position has no stop-loss.
	sl, tp := snapshotSLTPDecimal(ps.ProposedSL, ps.ProposedTP)
	pos := models.Position{
		Symbol: ps.Symbol, Side: models.TradeSideBuy,
		Quantity: filledQty, AvgEntryPrice: resp.AvgFillPrice, IsOpen: true,
		TotalCost: entryCost, TotalFees: entryFee,
		StopLoss: sl, TakeProfit: tp,
	}
	if err := r.db.WithContext(ctx).Create(&pos).Error; err != nil {
		log.Printf("[reconciler] id=%d position insert failed: %v", ps.ID, err)
	}
	r.broadcastRecovered(ps, "filled", trade.ID)
	return nil
}

func (r *ExchangeReconciler) recoverClose(ctx context.Context, ps models.PendingSignal, resp execution.OrderResponse) error {
	// CRY-31: settle through the same transactional close path as the
	// live executor — one copy of the PnL math, partial fills keep the
	// remainder open, and exit_reason is recorded.
	exitFee, exitFeeUnvalued := feeFromFillsValued(resp.Fills, quoteAsset, baseAssetOf(ps.Symbol, quoteAsset), tickerPricer(ctx, r.provider))
	if exitFeeUnvalued {
		log.Printf("[reconciler] WARN: recovered close %s — commission in an asset that can't be valued in %s (e.g. BNB); ExitFee may understate cost", resp.ClientOrderID, quoteAsset)
	}
	res, err := settleClose(ctx, r.db, closeFillInput{
		Symbol:          ps.Symbol,
		ExitPrice:       resp.AvgFillPrice,
		ExitQty:         resp.FilledQty,
		ExitFee:         exitFee,
		DustQty:         minQtyFor(ctx, r.provider, ps.Symbol),
		ExitReason:      models.ExitReasonAISell,
		AIConfidence:    ps.Confidence,
		AuditLogID:      ps.AuditLogID,
		ExchangeOrderID: resp.ExchangeOrderID,
		ClientOrderID:   resp.ClientOrderID,
	})
	if errors.Is(err, ErrAlreadyClosed) {
		// The position was closed by another path while this SELL was
		// in flight — the venue fill is orphaned inventory movement.
		return r.recordFailedRecovery(ctx, ps, resp, "already_closed",
			fmt.Sprintf("position closed by another path before SELL fill; orphan qty=%s", resp.FilledQty))
	}
	if err != nil {
		return fmt.Errorf("settle recovered close: %w", err)
	}
	log.Printf("[reconciler] recovered CLOSE: sym=%s exit_cost=%s exit_fee=%s pnl=%s net_pnl=%s remaining=%s trade_id=%d client=%s",
		ps.Symbol, res.ExitCost, exitFee, res.PnL, res.NetPnL, res.RemainingQty, res.TradeID, resp.ClientOrderID)
	if err := r.sm.MarkSubmitted(ctx, ps.ID, res.TradeID); err != nil {
		log.Printf("[reconciler] id=%d MarkSubmitted err: %v", ps.ID, err)
		return nil
	}
	r.broadcastRecovered(ps, "filled", res.TradeID)
	return nil
}

// failWithoutTrade transitions the pending_signal to failed without
// creating a trade row — used for not_placed and CANCELED/EXPIRED/
// REJECTED outcomes where there is no fill to record.
func (r *ExchangeReconciler) failWithoutTrade(ctx context.Context, ps models.PendingSignal, reason, detail string) error {
	full := reason + ": " + detail
	if err := r.sm.MarkFailed(ctx, ps.ID, full); err != nil {
		return fmt.Errorf("MarkFailed: %w", err)
	}
	r.broadcastRecovered(ps, reason, 0)
	return nil
}

// recordFailedRecovery is the slippage-exceeded-on-recovery path —
// Binance did fill the order, but the price moved outside our cap.
// We persist a FAILED trade row (so the audit trail captures the fill
// details) and transition the pending_signal to failed.
func (r *ExchangeReconciler) recordFailedRecovery(ctx context.Context, ps models.PendingSignal, resp execution.OrderResponse, reason, detail string) error {
	side := models.TradeSideBuy
	if ps.Action == "SELL" {
		side = models.TradeSideSell
	}
	// CRY-9: even on a slippage-failed recovery, Binance actually
	// charged a fee on the fill — record EntryCost + EntryFee for the
	// audit trail. NetPnL stays 0 (the position never reaches close).
	entryCost := resp.AvgFillPrice.Mul(resp.FilledQty)
	// EXP-1: value any base-asset commission in USDT for the audit trail.
	// FAILED rows are never closed, so no quantity netting is needed here.
	entryFee, _ := feeFromFillsValued(resp.Fills, quoteAsset, baseAssetOf(ps.Symbol, quoteAsset), tickerPricer(ctx, r.provider))
	trade := models.Trade{
		Symbol:          ps.Symbol,
		Side:            side,
		Status:          models.TradeStatusFailed,
		EntryPrice:      resp.AvgFillPrice,
		Quantity:        resp.FilledQty,
		EntryCost:       entryCost,
		EntryFee:        entryFee,
		FeeCurrency:     "USDT",
		ExchangeOrderID: resp.ExchangeOrderID,
		ClientOrderID:   resp.ClientOrderID,
		AIConfidence:    ps.Confidence,
		AIReasoning:     ps.Reasoning,
		Error:           reason + ": " + detail,
		EntryAt:         time.Now(),
		IsPaper:         false, // reconciler is live-only (see comment in handleFilled above)
	}
	if err := r.db.WithContext(ctx).Create(&trade).Error; err != nil {
		log.Printf("[reconciler] id=%d could not persist FAILED recovery trade: %v", ps.ID, err)
	}
	if err := r.sm.MarkFailed(ctx, ps.ID, reason+": "+detail); err != nil {
		return fmt.Errorf("MarkFailed: %w", err)
	}
	r.broadcastRecovered(ps, reason, trade.ID)
	return nil
}

func (r *ExchangeReconciler) broadcastRecovered(ps models.PendingSignal, reason string, tradeID uint) {
	if r.notifier == nil {
		return
	}
	payload := map[string]any{
		"pending_signal_id": ps.ID,
		"symbol":            ps.Symbol,
		"action":            ps.Action,
		"reason":            reason,
	}
	if tradeID != 0 {
		payload["trade_id"] = tradeID
	}
	r.notifier.Broadcast(map[string]any{
		"type": "signal.crash_recovered",
		"data": payload,
	})
}

// checkSlippage mirrors Executor.slippageExceeded so the reconciler
// applies the SAME cap a live Submit would have. Standard, not a
// bandaid: recovery doesn't get a free pass on slippage just because
// the fill predates the crash recovery.
func (r *ExchangeReconciler) checkSlippage(expected, actual decimal.Decimal) (bool, decimal.Decimal) {
	if r.maxSlippageBps <= 0 || expected.IsZero() || actual.IsZero() {
		return false, decimal.Zero
	}
	diff := actual.Sub(expected).Abs()
	bps := diff.Div(expected).Mul(decimal.NewFromInt(10000))
	return bps.GreaterThan(decimal.NewFromInt(r.maxSlippageBps)), bps
}

var _ approval.Reconciler = (*ExchangeReconciler)(nil)
