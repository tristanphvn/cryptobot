package executor

import (
	"context"
	"testing"
	"time"

	"cryptobot/internal/approval"
	"cryptobot/internal/database/models"
	"cryptobot/internal/execution"

	"github.com/shopspring/decimal"
)

// EXP-1/EXP-2 on the crash-recovery path. recoverOpen duplicates submitOpen's
// fee/quantity logic: a Spot MARKET BUY is commissioned in the BASE asset (BTC),
// so recovery must value that commission in USDT (EXP-1) and net it out of the
// stored quantity (EXP-2) so a later close SELL sizes against real holdings.
//
// The existing happy-path recovery test (TestReconciler_Filled_...) only feeds
// a USDT commission, where netting is a no-op and the base-valuation branch is
// never taken. This pins the reconciler's duplicated logic against a silent
// revert to feeFromFills(fills,"USDT") + gross quantity.
func TestReconciler_Filled_BaseAssetCommissionValuedAndNetted(t *testing.T) {
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
			AvgFillPrice:    decimal.NewFromInt(70000),
			Fills: []execution.Fill{
				{
					Price:           decimal.NewFromInt(70000),
					Qty:             decimal.RequireFromString("0.001"),
					Commission:      decimal.RequireFromString("0.000001"),
					CommissionAsset: "BTC",
				},
			},
		},
	}
	rec := NewExchangeReconciler(db, sm, provider, &fakeNotifier{}, 50)
	if err := rec.Reconcile(context.Background(), *ps); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.TradeID == nil || *got.TradeID == 0 {
		t.Fatal("expected trade_id linked to pending_signal")
	}
	var trade models.Trade
	db.First(&trade, *got.TradeID)

	// EXP-1: 0.000001 BTC * 70000 = 0.07 USDT (no longer recorded as 0).
	if want := decimal.RequireFromString("0.07"); !trade.EntryFee.Equal(want) {
		t.Fatalf("entry_fee=%s want %s (base commission valued in USDT)", trade.EntryFee, want)
	}
	// EXP-2: quantity = 0.001 - 0.000001 = 0.000999 (net of base commission).
	if want := decimal.RequireFromString("0.000999"); !trade.Quantity.Equal(want) {
		t.Fatalf("trade qty=%s want %s (net of base commission)", trade.Quantity, want)
	}
	// EntryCost stays GROSS: 70000 * 0.001 = 70 (cost-basis denominator).
	if want := decimal.RequireFromString("70"); !trade.EntryCost.Equal(want) {
		t.Fatalf("entry_cost=%s want %s (gross USDT deployed)", trade.EntryCost, want)
	}
	if trade.FeeCurrency != "USDT" {
		t.Fatalf("fee_currency=%s want USDT", trade.FeeCurrency)
	}

	var pos models.Position
	if err := db.Where("symbol = ?", "BTCUSDT").First(&pos).Error; err != nil {
		t.Fatalf("load position: %v", err)
	}
	if want := decimal.RequireFromString("0.000999"); !pos.Quantity.Equal(want) {
		t.Fatalf("position qty=%s want %s (net of base commission)", pos.Quantity, want)
	}
}
