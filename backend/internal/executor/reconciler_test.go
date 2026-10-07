package executor

import (
	"context"
	"strings"
	"testing"
	"time"

	"cryptobot/internal/approval"
	"cryptobot/internal/database/models"
	"cryptobot/internal/execution"

	"github.com/shopspring/decimal"
)

func seedExecutingPS(t *testing.T, sm *approval.StateMachine, action string) *models.PendingSignal {
	t.Helper()
	ps, err := sm.Enqueue(context.Background(), approval.EnqueueInput{
		Symbol:      "BTCUSDT",
		Interval:    "1h",
		AuditLogID:  1,
		Action:      action,
		Confidence:  0.9,
		Reasoning:   "test",
		MarketPrice: decimal.NewFromInt(70000),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sm.Approve(context.Background(), ps.ID, approval.Decision{By: "op", Source: "api"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := sm.MarkExecuting(context.Background(), ps.ID); err != nil {
		t.Fatalf("mark executing: %v", err)
	}
	return ps
}

func TestReconciler_Filled_RecordsTradeAndMarksSubmitted(t *testing.T) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: time.Minute, QueueMax: 5})
	ps := seedExecutingPS(t, sm, "BUY")

	provider := &fakeProvider{
		getOrderResp: execution.OrderResponse{
			ClientOrderID:   "cry5-" + uintStr(ps.ID),
			ExchangeOrderID: 12345,
			Symbol:          "BTCUSDT",
			Status:          execution.StatusFilled,
			FilledQty:       decimal.RequireFromString("0.001"),
			AvgFillPrice:    decimal.RequireFromString("70014"),
			Fills: []execution.Fill{
				// CRY-9: recovery must capture fee data, not just price.
				{Commission: decimal.RequireFromString("0.07"), CommissionAsset: "USDT"},
			},
		},
	}
	notifier := &fakeNotifier{}
	rec := NewExchangeReconciler(db, sm, provider, notifier, 50)

	if err := rec.Reconcile(context.Background(), *ps); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalSubmitted {
		t.Fatalf("ps.status=%s want submitted", got.Status)
	}
	if got.TradeID == nil || *got.TradeID == 0 {
		t.Fatal("expected trade_id linked to pending_signal")
	}
	var trade models.Trade
	db.First(&trade, *got.TradeID)
	if trade.Status != models.TradeStatusOpen {
		t.Fatalf("trade.status=%s want OPEN", trade.Status)
	}
	if !trade.EntryPrice.Equal(decimal.RequireFromString("70014")) {
		t.Fatalf("entry_price=%s want 70014", trade.EntryPrice)
	}
	// CRY-9: fee + cost must propagate from the provider Fills, not stay 0.
	wantCost := decimal.RequireFromString("70.014")
	if !trade.EntryCost.Equal(wantCost) {
		t.Fatalf("entry_cost=%s want %s", trade.EntryCost, wantCost)
	}
	if !trade.EntryFee.Equal(decimal.RequireFromString("0.07")) {
		t.Fatalf("entry_fee=%s want 0.07 (USDT fill commission)", trade.EntryFee)
	}
	if trade.FeeCurrency != "USDT" {
		t.Fatalf("fee_currency=%s want USDT", trade.FeeCurrency)
	}
	events := notifier.byType("signal.crash_recovered")
	if len(events) != 1 {
		t.Fatalf("crash_recovered events=%d want 1", len(events))
	}
}

func TestReconciler_OrderNotFound_MarksFailedNotPlaced(t *testing.T) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: time.Minute, QueueMax: 5})
	ps := seedExecutingPS(t, sm, "BUY")

	provider := &fakeProvider{getOrderErr: execution.ErrOrderNotFound}
	notifier := &fakeNotifier{}
	rec := NewExchangeReconciler(db, sm, provider, notifier, 50)

	if err := rec.Reconcile(context.Background(), *ps); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalFailed {
		t.Fatalf("status=%s want failed", got.Status)
	}
	if !strings.HasPrefix(got.RejectedReason, "not_placed") {
		t.Fatalf("reason=%q want not_placed prefix", got.RejectedReason)
	}
	// No trade row should be created for an order that never reached Binance.
	var n int64
	db.Model(&models.Trade{}).Count(&n)
	if n != 0 {
		t.Fatalf("expected 0 trades, got %d", n)
	}
}

func TestReconciler_Canceled_MarksFailedWithReason(t *testing.T) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: time.Minute, QueueMax: 5})
	ps := seedExecutingPS(t, sm, "BUY")

	provider := &fakeProvider{
		getOrderResp: execution.OrderResponse{Status: execution.StatusCanceled},
	}
	rec := NewExchangeReconciler(db, sm, provider, nil, 50)

	if err := rec.Reconcile(context.Background(), *ps); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalFailed {
		t.Fatalf("status=%s want failed", got.Status)
	}
	if !strings.Contains(got.RejectedReason, "CANCELED") {
		t.Fatalf("reason=%q must reference CANCELED", got.RejectedReason)
	}
}

func TestReconciler_NonTerminal_LeavesRowInExecuting(t *testing.T) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: time.Minute, QueueMax: 5})
	ps := seedExecutingPS(t, sm, "BUY")

	provider := &fakeProvider{
		getOrderResp: execution.OrderResponse{Status: execution.StatusPartiallyFilled},
	}
	rec := NewExchangeReconciler(db, sm, provider, nil, 50)

	err := rec.Reconcile(context.Background(), *ps)
	if err == nil {
		t.Fatal("expected err so sweeper leaves row in executing")
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalExecuting {
		t.Fatalf("status=%s want executing (reconciler must NOT advance non-terminal)", got.Status)
	}
}

func TestReconciler_SlippageExceededOnRecovery_FailsWithFillTrace(t *testing.T) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: time.Minute, QueueMax: 5})
	ps := seedExecutingPS(t, sm, "BUY")

	provider := &fakeProvider{
		getOrderResp: execution.OrderResponse{
			Status:          execution.StatusFilled,
			ExchangeOrderID: 9,
			FilledQty:       decimal.RequireFromString("0.001"),
			AvgFillPrice:    decimal.NewFromInt(71000), // 143 bps drift, cap 50 → exceeds
		},
	}
	rec := NewExchangeReconciler(db, sm, provider, nil, 50)

	if err := rec.Reconcile(context.Background(), *ps); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var psGot models.PendingSignal
	db.First(&psGot, ps.ID)
	if psGot.Status != models.PendingSignalFailed {
		t.Fatalf("ps.status=%s want failed", psGot.Status)
	}
	var trades []models.Trade
	db.Find(&trades)
	if len(trades) != 1 || trades[0].Status != models.TradeStatusFailed {
		t.Fatalf("expected 1 FAILED trade (the fill trace), got %+v", trades)
	}
	if !strings.HasPrefix(trades[0].Error, "slippage_exceeded") {
		t.Fatalf("trade.error=%q want slippage_exceeded prefix", trades[0].Error)
	}
	if trades[0].ExchangeOrderID != 9 {
		t.Fatalf("FAILED trade must preserve Binance orderId for audit; got %d", trades[0].ExchangeOrderID)
	}
}

func TestReconciler_AmbiguousProviderError_LeavesRowAlone(t *testing.T) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: time.Minute, QueueMax: 5})
	ps := seedExecutingPS(t, sm, "BUY")

	provider := &fakeProvider{getOrderErr: execution.ErrRateLimited}
	rec := NewExchangeReconciler(db, sm, provider, nil, 50)

	err := rec.Reconcile(context.Background(), *ps)
	if err == nil {
		t.Fatal("expected err on ambiguous so sweeper can retry next tick")
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalExecuting {
		t.Fatalf("status=%s want executing (ambiguous must not finalise)", got.Status)
	}
}

// uintStr is a tiny helper used only by tests; the production
// clientOrderIDFor uses fmt.Sprintf("cry5-%d", id) which we can't reach
// here without an import — replicate the format inline.
func uintStr(u uint) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
