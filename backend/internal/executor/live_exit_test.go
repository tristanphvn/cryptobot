package executor

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cryptobot/internal/approval"
	"cryptobot/internal/database/models"
	"cryptobot/internal/execution"
	"cryptobot/internal/marketprice"
	"cryptobot/internal/riskmanager"
)

type liveExitHarness struct {
	exec     *Executor
	mon      *PositionMonitor
	store    *marketprice.Store
	provider *fakeProvider
	notifier *fakeNotifier
}

// newLiveExitHarness: live executor + monitor with one open BTCUSDT
// position (0.001 @ 70000, SL 69000, TP 72000).
func newLiveExitHarness(t *testing.T) *liveExitHarness {
	t.Helper()
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: 30 * time.Second, QueueMax: 5})
	risk := riskmanager.New(db, riskmanager.Config{MaxPositionPct: 2, DailyDrawdownPct: 5, MaxOpenPositions: 3})
	provider := &fakeProvider{}
	notifier := &fakeNotifier{}
	exec := NewExecutor(db, nil, risk, ModeLive, sm).WithProvider(provider, 50).WithNotifier(notifier)
	store := marketprice.New()
	mon := NewPositionMonitor(db, exec, store, MonitorConfig{Interval: time.Second, MaxPriceAge: time.Minute})
	seedOpenPosition(t, exec, "BTCUSDT", dec("70000"), dec("0.001"), dec("69000"), dec("72000"))
	return &liveExitHarness{exec: exec, mon: mon, store: store, provider: provider, notifier: notifier}
}

func (h *liveExitHarness) tick(price string) {
	h.store.Set("BTCUSDT", dec(price), time.Now())
	h.mon.evaluateOnce(context.Background())
}

// expireBackoff lets the next tick retry immediately.
func (h *liveExitHarness) expireBackoff() {
	h.exec.exits.mu.Lock()
	for _, st := range h.exec.exits.state {
		st.nextAt = time.Time{}
	}
	h.exec.exits.mu.Unlock()
}

func (h *liveExitHarness) position(t *testing.T) (models.Position, bool) {
	t.Helper()
	return openPosition(t, h.exec)
}

func TestLiveExit_StopLossSellsOnVenueThenCloses(t *testing.T) {
	h := newLiveExitHarness(t)
	pos, _ := h.position(t)
	h.provider.placeResp = filledSell(execution.StatusFilled, "0.001", "68950",
		execution.Fill{Price: dec("68950"), Qty: dec("0.001"), Commission: dec("0.06895"), CommissionAsset: "USDT"})

	h.tick("68990")

	if len(h.provider.placeCalls) != 1 {
		t.Fatalf("SELL orders placed=%d want 1", len(h.provider.placeCalls))
	}
	req := h.provider.placeCalls[0]
	if req.Side != execution.SideSell || !req.Quantity.Equal(dec("0.001")) {
		t.Fatalf("order side=%s qty=%s want SELL 0.001", req.Side, req.Quantity)
	}
	if want := fmt.Sprintf("exit-%d-0", pos.ID); req.ClientOrderID != want {
		t.Fatalf("client id=%s want %s", req.ClientOrderID, want)
	}
	if _, open := h.position(t); open {
		t.Fatal("position should be closed after the venue fill")
	}
	var trade models.Trade
	h.exec.db.Where("symbol = ? AND status = ?", "BTCUSDT", models.TradeStatusClosed).First(&trade)
	if trade.ExitReason != models.ExitReasonStopLoss {
		t.Fatalf("exit_reason=%s want STOP_LOSS", trade.ExitReason)
	}
	// Booked at the venue fill and its real fee, not the trigger quote.
	if !trade.ExitPrice.Equal(dec("68950")) || !trade.ExitFee.Equal(dec("0.06895")) {
		t.Fatalf("exit_price=%s exit_fee=%s want 68950 / 0.06895", trade.ExitPrice, trade.ExitFee)
	}
}

func TestLiveExit_VenueRejects_PositionStaysOpenAndBacksOff(t *testing.T) {
	h := newLiveExitHarness(t)
	pos, _ := h.position(t)
	h.provider.placeErr = fmt.Errorf("%w: -2010", execution.ErrInsufficientBalance)

	h.tick("68990")

	if _, open := h.position(t); !open {
		t.Fatal("position must stay OPEN when the venue SELL fails")
	}
	var closed int64
	h.exec.db.Model(&models.Trade{}).Where("status = ?", models.TradeStatusClosed).Count(&closed)
	if closed != 0 {
		t.Fatalf("closed trades=%d want 0", closed)
	}
	events := h.notifier.byType("position.exit_failed")
	if len(events) != 1 {
		t.Fatalf("exit_failed events=%d want 1", len(events))
	}
	var audit models.AuditLog
	h.exec.db.Where("interval = ?", "monitor").Order("id DESC").First(&audit)
	if audit.BlockedBy != "execution_failed:insufficient_balance" {
		t.Fatalf("audit blocked_by=%q", audit.BlockedBy)
	}
	// CRY-34: the failure is readable for the positions API.
	f, ok := h.exec.ExitFailure(pos.Symbol, pos.ID)
	if !ok || f.Attempt != 1 || f.Reason != "insufficient_balance" || f.ExitReason != models.ExitReasonStopLoss ||
		f.NextRetryAt.IsZero() || f.LastFailedAt.IsZero() {
		t.Fatalf("exit failure=%+v ok=%v", f, ok)
	}
	if _, ok := h.exec.ExitFailure(pos.Symbol, pos.ID+1); ok {
		t.Fatal("another position on the symbol must not report this failure")
	}

	// Backing off: next tick neither sells nor writes another audit row.
	h.tick("68990")
	if len(h.provider.placeCalls) != 1 {
		t.Fatalf("SELL orders=%d during backoff, want 1", len(h.provider.placeCalls))
	}
	var audits int64
	h.exec.db.Model(&models.AuditLog{}).Where("interval = ?", "monitor").Count(&audits)
	if audits != 1 {
		t.Fatalf("monitor audit rows=%d during backoff, want 1", audits)
	}

	// After backoff: retry with a new client id, succeed.
	h.expireBackoff()
	h.provider.placeErr = nil
	h.provider.placeResp = filledSell(execution.StatusFilled, "0.001", "68980")
	h.tick("68990")
	if len(h.provider.placeCalls) != 2 {
		t.Fatalf("SELL orders=%d want 2", len(h.provider.placeCalls))
	}
	if want := fmt.Sprintf("exit-%d-1", pos.ID); h.provider.placeCalls[1].ClientOrderID != want {
		t.Fatalf("retry client id=%s want %s", h.provider.placeCalls[1].ClientOrderID, want)
	}
	if _, open := h.position(t); open {
		t.Fatal("position should close after successful retry")
	}
	if _, ok := h.exec.ExitFailure(pos.Symbol, pos.ID); ok {
		t.Fatal("exit failure must clear once the SELL fills")
	}
}

func TestExitBackoff_Grows(t *testing.T) {
	tr := newExitTracker()
	now := time.Now()
	_, n1 := tr.fail("BTCUSDT", 1, "", ExitFailure{}, now)
	_, n2 := tr.fail("BTCUSDT", 1, "", ExitFailure{}, now)
	if n1.Sub(now) != 5*time.Second || n2.Sub(now) != 10*time.Second {
		t.Fatalf("backoff %s, %s want 5s, 10s", n1.Sub(now), n2.Sub(now))
	}
	for i := 0; i < 20; i++ {
		_, n2 = tr.fail("BTCUSDT", 1, "", ExitFailure{}, now)
	}
	if n2.Sub(now) != exitBackoffMax {
		t.Fatalf("backoff %s want cap %s", n2.Sub(now), exitBackoffMax)
	}
	// New position on the symbol starts clean.
	if !tr.ready("BTCUSDT", 2, now) {
		t.Fatal("new position must not inherit the old backoff")
	}
}

// A timed-out SELL may have reached the venue: the retry must look it up
// under the same client id instead of selling again.
func TestLiveExit_AmbiguousOrder_LookedUpNotDoubled(t *testing.T) {
	h := newLiveExitHarness(t)
	pos, _ := h.position(t)
	h.provider.placeErr = execution.ErrPollTimeout

	h.tick("68990")
	if _, open := h.position(t); !open {
		t.Fatal("position must stay open while the outcome is unknown")
	}

	h.expireBackoff()
	h.provider.placeErr = nil
	h.provider.getOrderResp = filledSell(execution.StatusFilled, "0.001", "68970")
	h.tick("68990")

	wantID := fmt.Sprintf("exit-%d-0", pos.ID)
	if len(h.provider.getOrderIDs) != 1 || h.provider.getOrderIDs[0] != wantID {
		t.Fatalf("GetOrder ids=%v want [%s]", h.provider.getOrderIDs, wantID)
	}
	if len(h.provider.placeCalls) != 1 {
		t.Fatalf("SELL orders=%d want 1 (no second order)", len(h.provider.placeCalls))
	}
	if _, open := h.position(t); open {
		t.Fatal("position should close from the looked-up fill")
	}
}

func TestLiveExit_AmbiguousOrderNotFound_PlacesFresh(t *testing.T) {
	h := newLiveExitHarness(t)
	h.provider.placeErr = execution.ErrAmbiguousPlacement
	h.tick("68990")

	h.expireBackoff()
	h.provider.placeErr = nil
	h.provider.getOrderErr = execution.ErrOrderNotFound
	h.provider.placeResp = filledSell(execution.StatusFilled, "0.001", "68970")
	h.tick("68990")

	if len(h.provider.placeCalls) != 2 {
		t.Fatalf("SELL orders=%d want 2 (first never reached venue)", len(h.provider.placeCalls))
	}
	if _, open := h.position(t); open {
		t.Fatal("position should close")
	}
}

func TestLiveExit_PartialFill_RemainderSoldNextTick(t *testing.T) {
	h := newLiveExitHarness(t)
	h.provider.placeResp = filledSell(execution.StatusExpired, "0.0006", "68950")
	h.tick("68990")

	pos, open := h.position(t)
	if !open || !pos.Quantity.Equal(dec("0.0004")) {
		t.Fatalf("remaining open=%v qty=%s want 0.0004", open, pos.Quantity)
	}

	h.provider.placeResp = filledSell(execution.StatusFilled, "0.0004", "68940")
	h.tick("68990")
	if len(h.provider.placeCalls) != 2 || !h.provider.placeCalls[1].Quantity.Equal(dec("0.0004")) {
		t.Fatalf("second SELL calls=%d, want 1 more for 0.0004", len(h.provider.placeCalls))
	}
	if _, open := h.position(t); open {
		t.Fatal("position should be fully closed after selling the remainder")
	}
	var closed int64
	h.exec.db.Model(&models.Trade{}).Where("status = ?", models.TradeStatusClosed).Count(&closed)
	if closed != 2 {
		t.Fatalf("closed trades=%d want 2 (one per fill)", closed)
	}
}

// An approved AI SELL must not run while a monitor exit holds the slot.
func TestSubmitClose_BlockedWhileExitInFlight(t *testing.T) {
	provider := &fakeProvider{placeResp: filledSell(execution.StatusFilled, "0.001", "70000")}
	exec, _, _, _, ps := newSubmittableExecutor(t, provider, "SELL", 200)
	seedLiveOpen(t, exec, "0.001", "70000", "0")
	pos, _ := openPosition(t, exec)

	if !exec.exits.acquire("BTCUSDT") {
		t.Fatal("acquire")
	}
	_, err := exec.submitClose(context.Background(), ps, &pos)
	exec.exits.release("BTCUSDT")
	if err == nil || len(provider.placeCalls) != 0 {
		t.Fatalf("err=%v orders=%d — want exit_in_flight and no order", err, len(provider.placeCalls))
	}
}

// Approval SELL after the monitor already closed: nothing is placed.
func TestSubmitClose_PositionGone_NoOrder(t *testing.T) {
	provider := &fakeProvider{placeResp: filledSell(execution.StatusFilled, "0.001", "70000")}
	exec, _, _, _, ps := newSubmittableExecutor(t, provider, "SELL", 200)
	stale := models.Position{Symbol: "BTCUSDT", Quantity: dec("0.001"), AvgEntryPrice: dec("70000")}

	if _, err := exec.submitClose(context.Background(), ps, &stale); err == nil {
		t.Fatal("expected already_closed failure")
	}
	if len(provider.placeCalls) != 0 {
		t.Fatalf("orders=%d want 0", len(provider.placeCalls))
	}
}
