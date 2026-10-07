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

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func filledBuy(qty, price string, fills ...execution.Fill) execution.OrderResponse {
	return execution.OrderResponse{
		ClientOrderID: "cry5-1", ExchangeOrderID: 1, Symbol: "BTCUSDT",
		Side: execution.SideBuy, Status: execution.StatusFilled,
		FilledQty: dec(qty), AvgFillPrice: dec(price), Fills: fills,
	}
}

func filledSell(status execution.BinanceStatus, qty, price string, fills ...execution.Fill) execution.OrderResponse {
	return execution.OrderResponse{
		ClientOrderID: "cry5-1", ExchangeOrderID: 2, Symbol: "BTCUSDT",
		Side: execution.SideSell, Status: status,
		FilledQty: dec(qty), AvgFillPrice: dec(price), Fills: fills,
	}
}

// seedLiveOpen writes an OPEN trade + position the way submitOpen would.
func seedLiveOpen(t *testing.T, exec *Executor, qty, entry, entryFee string) {
	t.Helper()
	q, p := dec(qty), dec(entry)
	if err := exec.db.Create(&models.Trade{
		Symbol: "BTCUSDT", Side: models.TradeSideBuy, Status: models.TradeStatusOpen,
		EntryPrice: p, Quantity: q, EntryCost: p.Mul(q), EntryFee: dec(entryFee),
		FeeCurrency: "USDT", EntryAt: time.Now(),
	}).Error; err != nil {
		t.Fatalf("seed trade: %v", err)
	}
	if err := exec.db.Create(&models.Position{
		Symbol: "BTCUSDT", Side: models.TradeSideBuy, Quantity: q, AvgEntryPrice: p, IsOpen: true,
	}).Error; err != nil {
		t.Fatalf("seed position: %v", err)
	}
}

func openPosition(t *testing.T, exec *Executor) (models.Position, bool) {
	t.Helper()
	var pos models.Position
	err := exec.db.Where("symbol = ? AND is_open = ?", "BTCUSDT", true).First(&pos).Error
	return pos, err == nil
}

// ─────────────────────────────────────────────────────────────────────
// BUY — base-asset commission
// ─────────────────────────────────────────────────────────────────────

// Binance Spot with BNB-fee off charges a BUY in BTC: the wallet holds
// FilledQty − commission, and the fee must be valued in USDT.
func TestSubmitOpen_BaseAssetCommission(t *testing.T) {
	provider := &fakeProvider{placeResp: filledBuy("0.00286", "70000",
		execution.Fill{Price: dec("70000"), Qty: dec("0.00286"), Commission: dec("0.00000286"), CommissionAsset: "BTC"})}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "BUY", 50)

	tradeID, err := exec.submitOpen(context.Background(), ps)
	if err != nil {
		t.Fatalf("submitOpen: %v", err)
	}
	var trade models.Trade
	db.First(&trade, tradeID)
	if !trade.EntryFee.Equal(dec("0.2002")) {
		t.Fatalf("entry_fee=%s want 0.2002 (0.00000286 BTC × 70000)", trade.EntryFee)
	}
	if !trade.EntryCost.Equal(dec("200.2")) {
		t.Fatalf("entry_cost=%s want 200.2 (USDT spent)", trade.EntryCost)
	}
	if !trade.Quantity.Equal(dec("0.00285714")) {
		t.Fatalf("trade qty=%s want 0.00285714 (held)", trade.Quantity)
	}
	pos, ok := openPosition(t, exec)
	if !ok || !pos.Quantity.Equal(dec("0.00285714")) {
		t.Fatalf("position qty=%s want 0.00285714 — must match wallet, not FilledQty", pos.Quantity)
	}
}

// Round trip on a base-commission BUY: net PnL equals what the wallet
// actually gained (USDT received − USDT spent − exit fee).
func TestRoundTrip_BaseCommission_NetMatchesWallet(t *testing.T) {
	provider := &fakeProvider{placeResp: filledBuy("0.002", "50000",
		execution.Fill{Price: dec("50000"), Qty: dec("0.002"), Commission: dec("0.000002"), CommissionAsset: "BTC"})}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "BUY", 0) // prices differ from the 70000 snapshot
	if _, err := exec.submitOpen(context.Background(), ps); err != nil {
		t.Fatalf("submitOpen: %v", err)
	}
	pos, _ := openPosition(t, exec)

	// Sell the held 0.001998 at 51000, fee 0.1% in USDT.
	provider.placeResp = filledSell(execution.StatusFilled, "0.001998", "51000",
		execution.Fill{Price: dec("51000"), Qty: dec("0.001998"), Commission: dec("0.101898"), CommissionAsset: "USDT"})
	tradeID, err := exec.submitClose(context.Background(), ps, &pos)
	if err != nil {
		t.Fatalf("submitClose: %v", err)
	}
	var trade models.Trade
	db.First(&trade, tradeID)

	// Wallet: spent 100 USDT, received 0.001998 × 51000 − 0.101898.
	wallet := dec("0.001998").Mul(dec("51000")).Sub(dec("0.101898")).Sub(dec("100"))
	if !trade.NetPnL.Equal(wallet) {
		t.Fatalf("net_pnl=%s want %s (wallet delta)", trade.NetPnL, wallet)
	}
	if _, open := openPosition(t, exec); open {
		t.Fatal("position should be fully closed")
	}
}

// ─────────────────────────────────────────────────────────────────────
// SELL — partial fills and dust
// ─────────────────────────────────────────────────────────────────────

func TestSubmitClose_PartialFill_KeepsRemainderOpen(t *testing.T) {
	provider := &fakeProvider{placeResp: filledSell(execution.StatusFilled, "0.0004", "71000",
		execution.Fill{Commission: dec("0.0284"), CommissionAsset: "USDT"})}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "SELL", 200) // fills within 2% of the 70000 snapshot
	seedLiveOpen(t, exec, "0.001", "70000", "0.07")
	pos, _ := openPosition(t, exec)

	tradeID, err := exec.submitClose(context.Background(), ps, &pos)
	if err != nil {
		t.Fatalf("submitClose: %v", err)
	}

	after, open := openPosition(t, exec)
	if !open || !after.Quantity.Equal(dec("0.0006")) {
		t.Fatalf("position open=%v qty=%s want open with 0.0006", open, after.Quantity)
	}

	var closed models.Trade
	db.First(&closed, tradeID)
	if closed.Status != models.TradeStatusClosed || !closed.Quantity.Equal(dec("0.0004")) {
		t.Fatalf("closed trade status=%s qty=%s want CLOSED 0.0004", closed.Status, closed.Quantity)
	}
	// 40% of entry cost 70 and fee 0.07.
	if !closed.EntryCost.Equal(dec("28")) || !closed.EntryFee.Equal(dec("0.028")) {
		t.Fatalf("closed entry_cost=%s entry_fee=%s want 28 / 0.028", closed.EntryCost, closed.EntryFee)
	}
	// gross (71000−70000)×0.0004 = 0.4; net = 0.4 − 0.028 − 0.0284.
	if !closed.NetPnL.Equal(dec("0.3436")) {
		t.Fatalf("net_pnl=%s want 0.3436", closed.NetPnL)
	}

	var remaining models.Trade
	db.Where("symbol = ? AND status = ?", "BTCUSDT", models.TradeStatusOpen).First(&remaining)
	if !remaining.Quantity.Equal(dec("0.0006")) || !remaining.EntryCost.Equal(dec("42")) || !remaining.EntryFee.Equal(dec("0.042")) {
		t.Fatalf("open trade qty=%s cost=%s fee=%s want 0.0006 / 42 / 0.042",
			remaining.Quantity, remaining.EntryCost, remaining.EntryFee)
	}
}

// Remainder at or below LOT_SIZE minQty can't be sold — close fully.
func TestSubmitClose_DustRemainderClosesPosition(t *testing.T) {
	provider := &fakeProvider{
		placeResp: filledSell(execution.StatusFilled, "0.00285", "71000"),
		filters:   execution.SymbolFilters{MinQty: dec("0.00001"), StepSize: dec("0.00001")},
	}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "SELL", 200) // fills within 2% of the 70000 snapshot
	seedLiveOpen(t, exec, "0.00285714", "70000", "0.2")
	pos, _ := openPosition(t, exec)

	tradeID, err := exec.submitClose(context.Background(), ps, &pos)
	if err != nil {
		t.Fatalf("submitClose: %v", err)
	}
	if _, open := openPosition(t, exec); open {
		t.Fatal("dust remainder 0.00000714 ≤ minQty must close the position")
	}
	var trade models.Trade
	db.First(&trade, tradeID)
	if !trade.EntryFee.Equal(dec("0.2")) {
		t.Fatalf("entry_fee=%s want full 0.2 on full close", trade.EntryFee)
	}
}

// A MARKET SELL that EXPIRED after a partial fill still moved money.
func TestSubmitClose_ExpiredWithFill_SettlesFilledPart(t *testing.T) {
	provider := &fakeProvider{placeResp: filledSell(execution.StatusExpired, "0.0003", "71000")}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "SELL", 200) // fills within 2% of the 70000 snapshot
	seedLiveOpen(t, exec, "0.001", "70000", "0")
	pos, _ := openPosition(t, exec)

	tradeID, err := exec.submitClose(context.Background(), ps, &pos)
	if err != nil {
		t.Fatalf("submitClose: %v", err)
	}
	var trade models.Trade
	db.First(&trade, tradeID)
	if trade.Status != models.TradeStatusClosed || !trade.Quantity.Equal(dec("0.0003")) {
		t.Fatalf("trade status=%s qty=%s want CLOSED 0.0003", trade.Status, trade.Quantity)
	}
	if after, open := openPosition(t, exec); !open || !after.Quantity.Equal(dec("0.0007")) {
		t.Fatalf("remaining open=%v qty=%s want 0.0007", open, after.Quantity)
	}
}

func TestSubmitClose_ExpiredNoFill_StillFails(t *testing.T) {
	provider := &fakeProvider{placeResp: filledSell(execution.StatusExpired, "0", "0")}
	exec, _, _, _, ps := newSubmittableExecutor(t, provider, "SELL", 200) // fills within 2% of the 70000 snapshot
	seedLiveOpen(t, exec, "0.001", "70000", "0")
	pos, _ := openPosition(t, exec)

	if _, err := exec.submitClose(context.Background(), ps, &pos); err == nil {
		t.Fatal("expected failure for EXPIRED with zero fill")
	}
	if _, open := openPosition(t, exec); !open {
		t.Fatal("position must stay open when nothing filled")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Reconciler
// ─────────────────────────────────────────────────────────────────────

func TestReconciler_RecoverOpen_BaseCommissionAndSLTP(t *testing.T) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: time.Minute, QueueMax: 5})
	ps := seedExecutingPS(t, sm, "BUY")
	ps.ProposedSL = dec("68000")
	ps.ProposedTP = dec("73000")

	provider := &fakeProvider{getOrderResp: filledBuy("0.002", "70000",
		execution.Fill{Price: dec("70000"), Qty: dec("0.002"), Commission: dec("0.000002"), CommissionAsset: "BTC"})}
	rec := NewExchangeReconciler(db, sm, provider, &fakeNotifier{}, 50)
	if err := rec.Reconcile(context.Background(), *ps); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var pos models.Position
	if err := db.Where("symbol = ? AND is_open = ?", "BTCUSDT", true).First(&pos).Error; err != nil {
		t.Fatalf("load position: %v", err)
	}
	if !pos.Quantity.Equal(dec("0.001998")) {
		t.Fatalf("qty=%s want 0.001998", pos.Quantity)
	}
	if pos.StopLoss == nil || !pos.StopLoss.Equal(dec("68000")) || pos.TakeProfit == nil || !pos.TakeProfit.Equal(dec("73000")) {
		t.Fatalf("SL/TP not snapshotted: sl=%v tp=%v", pos.StopLoss, pos.TakeProfit)
	}
	var trade models.Trade
	db.Where("symbol = ?", "BTCUSDT").First(&trade)
	if !trade.EntryFee.Equal(dec("0.14")) {
		t.Fatalf("entry_fee=%s want 0.14", trade.EntryFee)
	}
}

func TestReconciler_RecoverClose_ExitReasonAndPartial(t *testing.T) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: time.Minute, QueueMax: 5})
	ps := seedExecutingPS(t, sm, "SELL")
	exec := NewExecutor(db, nil, nil, ModePaper, nil)
	seedLiveOpen(t, exec, "0.001", "70000", "0.07")

	// Order EXPIRED after selling half.
	provider := &fakeProvider{getOrderResp: filledSell(execution.StatusExpired, "0.0005", "70000")}
	rec := NewExchangeReconciler(db, sm, provider, &fakeNotifier{}, 50)
	if err := rec.Reconcile(context.Background(), *ps); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalSubmitted || got.TradeID == nil {
		t.Fatalf("ps status=%s trade=%v want submitted with trade", got.Status, got.TradeID)
	}
	var closed models.Trade
	db.First(&closed, *got.TradeID)
	if closed.ExitReason != models.ExitReasonAISell {
		t.Fatalf("exit_reason=%q want AI_SELL", closed.ExitReason)
	}
	if !closed.NetPnL.Equal(dec("-0.035")) {
		t.Fatalf("net_pnl=%s want -0.035 (half the entry fee)", closed.NetPnL)
	}
	if pos, open := openPosition(t, exec); !open || !pos.Quantity.Equal(dec("0.0005")) {
		t.Fatalf("remaining open=%v qty=%s want 0.0005", open, pos.Quantity)
	}
}
