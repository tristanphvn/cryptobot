package executor

import (
	"context"
	"sync"
	"testing"
	"time"

	"cryptobot/internal/database/models"
	"cryptobot/internal/marketprice"
	"cryptobot/internal/riskmanager"

	"github.com/shopspring/decimal"
)

// helper: build an executor in paper mode wired against an in-memory
// SQLite + a fresh marketprice store. Returns everything tests assert.
// Trailing-stop firing is disabled by default (TrailingStopPct=0) so
// existing SL/TP-only tests behave identically to pre-CRY-22.
func newMonitorHarness(t *testing.T) (*Executor, *PositionMonitor, *marketprice.Store) {
	t.Helper()
	db := newTestDB(t)
	risk := riskmanager.New(db, riskmanager.Config{
		MaxPositionPct: 2.0, DailyDrawdownPct: 5.0, MaxOpenPositions: 3,
	})
	exec := NewExecutor(db, nil, risk, ModePaper, nil)
	store := marketprice.New()
	mon := NewPositionMonitor(db, exec, store, MonitorConfig{
		Interval:    time.Second,
		MaxPriceAge: 5 * time.Minute,
	})
	return exec, mon, store
}

// newMonitorHarnessWithTrail builds the same harness but with trailing-
// stop firing enabled at the supplied parameters. Used by CRY-22 tests.
func newMonitorHarnessWithTrail(t *testing.T, trailPct, armPct, minLockedPct float64) (*Executor, *PositionMonitor, *marketprice.Store) {
	t.Helper()
	db := newTestDB(t)
	risk := riskmanager.New(db, riskmanager.Config{
		MaxPositionPct: 2.0, DailyDrawdownPct: 5.0, MaxOpenPositions: 3,
	})
	exec := NewExecutor(db, nil, risk, ModePaper, nil)
	store := marketprice.New()
	mon := NewPositionMonitor(db, exec, store, MonitorConfig{
		Interval:           time.Second,
		MaxPriceAge:        5 * time.Minute,
		TrailingStopPct:    trailPct,
		TrailingStopArmPct: armPct,
		MinLockedProfitPct: minLockedPct,
	})
	return exec, mon, store
}

// seedOpenPosition writes both a Position (with SL/TP) and a matching
// OPEN Trade row so closePosition's lookups succeed.
func seedOpenPosition(t *testing.T, exec *Executor, symbol string, entry, qty, sl, tp decimal.Decimal) {
	t.Helper()
	trade := models.Trade{
		Symbol: symbol, Side: models.TradeSideBuy, Status: models.TradeStatusOpen,
		EntryPrice: entry, Quantity: qty,
		EntryCost: entry.Mul(qty), EntryFee: decimal.Zero, FeeCurrency: "USDT",
		EntryAt: time.Now(), IsPaper: true,
	}
	if err := exec.db.Create(&trade).Error; err != nil {
		t.Fatalf("seed trade: %v", err)
	}
	slP, tpP := snapshotSLTPDecimal(sl, tp)
	pos := models.Position{
		Symbol: symbol, Side: models.TradeSideBuy,
		Quantity: qty, AvgEntryPrice: entry, IsOpen: true,
		StopLoss: slP, TakeProfit: tpP,
	}
	if err := exec.db.Create(&pos).Error; err != nil {
		t.Fatalf("seed position: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// evaluateExitRules — pure rule engine
// ─────────────────────────────────────────────────────────────────────

func TestEvaluateExitRules_NoSnapshot_NoTrigger(t *testing.T) {
	pos := models.Position{}
	r, fired := evaluateExitRules(pos, decimal.NewFromInt(100))
	if fired || r != "" {
		t.Fatalf("expected no fire on empty snapshot, got reason=%q fired=%v", r, fired)
	}
}

func TestEvaluateExitRules_TPHit(t *testing.T) {
	tp := decimal.NewFromInt(120)
	pos := models.Position{TakeProfit: &tp}
	cases := []struct {
		price decimal.Decimal
		fire  bool
	}{
		{decimal.NewFromInt(119), false},
		{decimal.NewFromInt(120), true}, // exact match triggers
		{decimal.NewFromInt(125), true},
	}
	for _, c := range cases {
		r, fired := evaluateExitRules(pos, c.price)
		if fired != c.fire {
			t.Fatalf("price=%s: fired=%v want %v (reason=%q)", c.price, fired, c.fire, r)
		}
		if fired && r != models.ExitReasonTakeProfit {
			t.Fatalf("price=%s: reason=%q want TAKE_PROFIT", c.price, r)
		}
	}
}

func TestEvaluateExitRules_SLHit(t *testing.T) {
	sl := decimal.NewFromInt(80)
	pos := models.Position{StopLoss: &sl}
	cases := []struct {
		price decimal.Decimal
		fire  bool
	}{
		{decimal.NewFromInt(81), false},
		{decimal.NewFromInt(80), true}, // exact match triggers
		{decimal.NewFromInt(75), true},
	}
	for _, c := range cases {
		r, fired := evaluateExitRules(pos, c.price)
		if fired != c.fire {
			t.Fatalf("price=%s: fired=%v want %v (reason=%q)", c.price, fired, c.fire, r)
		}
		if fired && r != models.ExitReasonStopLoss {
			t.Fatalf("price=%s: reason=%q want STOP_LOSS", c.price, r)
		}
	}
}

func TestEvaluateExitRules_StopLossPrecedence(t *testing.T) {
	// Pathological case: snapshot has TP=120, SL=130 (inverted). A spot
	// price of 100 is ≤ SL AND below TP. STOP_LOSS should win because
	// it reflects the immediate risk move.
	sl := decimal.NewFromInt(130)
	tp := decimal.NewFromInt(120)
	pos := models.Position{StopLoss: &sl, TakeProfit: &tp}
	r, fired := evaluateExitRules(pos, decimal.NewFromInt(100))
	if !fired || r != models.ExitReasonStopLoss {
		t.Fatalf("expected STOP_LOSS precedence, got reason=%q fired=%v", r, fired)
	}
}

// ─────────────────────────────────────────────────────────────────────
// CRY-22 — evaluateTrailingStop + evaluateCombinedExit
// ─────────────────────────────────────────────────────────────────────

func TestEvaluateTrailingStop_DisabledNeverFires(t *testing.T) {
	pos := models.Position{
		AvgEntryPrice: decimal.NewFromInt(100),
		Quantity:      decimal.NewFromInt(1),
	}
	// trailPct=0 disables firing regardless of state. Peak still tracked.
	newPeak, _, fired := evaluateTrailingStop(pos, decimal.NewFromInt(50),
		0, 1, 0.5, decimal.NewFromFloat(0.00075))
	if fired {
		t.Fatal("expected fired=false when trailPct=0")
	}
	if !newPeak.Equal(decimal.NewFromInt(100)) {
		t.Fatalf("expected newPeak=entry (100), got %s", newPeak)
	}
}

func TestEvaluateTrailingStop_ArmsAndFires(t *testing.T) {
	// Position opened at 100, qty 1. Peak hit 110 (10% up), price retraces
	// to 107.7 (2.09% drawdown from peak — past 2% trail). Net locked
	// after fees ~7.55% — easily above min 0.5%.
	peak := decimal.NewFromInt(110)
	maxPnL := decimal.NewFromInt(10)
	pos := models.Position{
		AvgEntryPrice:       decimal.NewFromInt(100),
		Quantity:            decimal.NewFromInt(1),
		PeakPrice:           &peak,
		MaxUnrealizedPnLPct: &maxPnL,
		TrailingStopArmed:   true,
	}
	newPeak, armed, fired := evaluateTrailingStop(pos, decimal.RequireFromString("107.7"),
		2.0, 1.0, 0.5, decimal.NewFromFloat(0.00075))
	if !fired {
		t.Fatalf("expected fired=true (10%% peak, 2.09%% drawdown), got fired=false (peak=%s armed=%v)", newPeak, armed)
	}
	if !armed {
		t.Fatal("expected armed=true (peak unrealized >> arm threshold)")
	}
}

func TestEvaluateTrailingStop_MinLockedProfitBlocks(t *testing.T) {
	// Peak 101.5 (1.5% up — armed), price retraces to 99.4 (~2.07% drawdown
	// from peak — past 2% trail). But locked PnL is NEGATIVE (sold at 99.4
	// after entry 100), so the min-locked-profit gate must block the fire.
	peak := decimal.RequireFromString("101.5")
	maxPnL := decimal.RequireFromString("1.5")
	pos := models.Position{
		AvgEntryPrice:       decimal.NewFromInt(100),
		Quantity:            decimal.NewFromInt(1),
		PeakPrice:           &peak,
		MaxUnrealizedPnLPct: &maxPnL,
		TrailingStopArmed:   true,
	}
	_, _, fired := evaluateTrailingStop(pos, decimal.RequireFromString("99.4"),
		2.0, 1.0, 0.5, decimal.NewFromFloat(0.00075))
	if fired {
		t.Fatal("expected fired=false (locked PnL below 0.5% min)")
	}
}

func TestEvaluateTrailingStop_NotArmedYet(t *testing.T) {
	// Peak only +0.5% (below 1% arm threshold). Drawdown wide but trail
	// shouldn't be armed yet.
	peak := decimal.RequireFromString("100.5")
	maxPnL := decimal.RequireFromString("0.5")
	pos := models.Position{
		AvgEntryPrice:       decimal.NewFromInt(100),
		Quantity:            decimal.NewFromInt(1),
		PeakPrice:           &peak,
		MaxUnrealizedPnLPct: &maxPnL,
	}
	_, armed, fired := evaluateTrailingStop(pos, decimal.NewFromInt(95),
		2.0, 1.0, 0.5, decimal.NewFromFloat(0.00075))
	if armed || fired {
		t.Fatalf("expected armed=false fired=false (peak unrealized 0.5%% < arm 1%%), got armed=%v fired=%v", armed, fired)
	}
}

func TestEvaluateCombinedExit_StopLossWinsOverTrail(t *testing.T) {
	// Same tick triggers both SL (immediate) and trailing (peak drawdown).
	// Hierarchy demands STOP_LOSS wins.
	sl := decimal.NewFromInt(95)
	pos := models.Position{StopLoss: &sl}
	r, fired := evaluateCombinedExit(pos, decimal.NewFromInt(94), true) // trailFired=true too
	if !fired || r != models.ExitReasonStopLoss {
		t.Fatalf("expected STOP_LOSS over TRAILING_STOP, got reason=%q fired=%v", r, fired)
	}
}

func TestEvaluateCombinedExit_TrailWinsOverTakeProfit(t *testing.T) {
	// TP would trigger (price >= 120) AND trailing fired. Trail wins.
	tp := decimal.NewFromInt(120)
	pos := models.Position{TakeProfit: &tp}
	r, fired := evaluateCombinedExit(pos, decimal.NewFromInt(125), true)
	if !fired || r != models.ExitReasonTrailingStop {
		t.Fatalf("expected TRAILING_STOP over TAKE_PROFIT, got reason=%q fired=%v", r, fired)
	}
}

// End-to-end: open position with NO SL/TP (the pre-CRY-22 invisible
// case), peak the price, retrace past trail threshold, expect a
// TRAILING_STOP-flagged close to appear.
func TestMonitor_TrailingStop_EndToEnd_NoSLTP(t *testing.T) {
	exec, mon, store := newMonitorHarnessWithTrail(t, 2.0, 1.0, 0.5)
	seedOpenPosition(t, exec, "BTCUSDT",
		decimal.NewFromInt(100), decimal.NewFromInt(1),
		decimal.Zero, decimal.Zero) // sl/tp both nil

	// Tick 1 — establish peak at 110 (10% up, well above arm threshold).
	store.Set("BTCUSDT", decimal.NewFromInt(110), time.Now())
	mon.evaluateOnce(context.Background())

	var posAtPeak models.Position
	exec.db.Where("symbol = ?", "BTCUSDT").First(&posAtPeak)
	if !posAtPeak.IsOpen {
		t.Fatal("position closed on peak tick — trail should only fire on drawdown")
	}
	if !posAtPeak.TrailingStopArmed {
		t.Fatal("expected trailing_stop_armed=true after 10%% peak")
	}
	if posAtPeak.PeakPrice == nil || !posAtPeak.PeakPrice.Equal(decimal.NewFromInt(110)) {
		t.Fatalf("expected peak_price=110, got %v", posAtPeak.PeakPrice)
	}

	// Tick 2 — price drops to 107.5 (2.27% drawdown from peak, past 2%).
	store.Set("BTCUSDT", decimal.RequireFromString("107.5"), time.Now())
	mon.evaluateOnce(context.Background())

	var trade models.Trade
	if err := exec.db.Where("symbol = ? AND status = ?", "BTCUSDT", models.TradeStatusClosed).First(&trade).Error; err != nil {
		t.Fatalf("expected CLOSED trade after trail fire: %v", err)
	}
	if trade.ExitReason != models.ExitReasonTrailingStop {
		t.Fatalf("exit_reason=%s want TRAILING_STOP", trade.ExitReason)
	}

	// Audit row should carry the trailing-specific reasoning string.
	var audits []models.AuditLog
	exec.db.Where("symbol = ?", "BTCUSDT").Order("id ASC").Find(&audits)
	if len(audits) == 0 {
		t.Fatal("expected at least one monitor audit row")
	}
	last := audits[len(audits)-1]
	if last.Action != "SELL" {
		t.Fatalf("audit_log.action=%s want SELL", last.Action)
	}
	if !contains(last.Reasoning, "TRAILING_STOP") || !contains(last.Reasoning, "peak=") || !contains(last.Reasoning, "locked_net=") {
		t.Fatalf("audit reasoning missing trail-specific fields: %q", last.Reasoning)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────
// evaluateOnce — end-to-end monitor sweep
// ─────────────────────────────────────────────────────────────────────

func TestMonitor_TakeProfitClosesPosition(t *testing.T) {
	exec, mon, store := newMonitorHarness(t)
	seedOpenPosition(t, exec, "BTCUSDT",
		decimal.NewFromInt(70000), decimal.RequireFromString("0.001"),
		decimal.NewFromInt(68000), decimal.NewFromInt(71000))

	// Price ticks above TP.
	store.Set("BTCUSDT", decimal.NewFromInt(71500), time.Now())
	mon.evaluateOnce(context.Background())

	var pos models.Position
	if err := exec.db.Where("symbol = ?", "BTCUSDT").First(&pos).Error; err != nil {
		t.Fatalf("load position: %v", err)
	}
	if pos.IsOpen {
		t.Fatal("position still OPEN after TP trigger")
	}
	var trade models.Trade
	if err := exec.db.Where("symbol = ? AND status = ?", "BTCUSDT", models.TradeStatusClosed).First(&trade).Error; err != nil {
		t.Fatalf("expected a CLOSED trade row: %v", err)
	}
	if trade.ExitReason != models.ExitReasonTakeProfit {
		t.Fatalf("exit_reason=%s want TAKE_PROFIT", trade.ExitReason)
	}
	if !trade.ExitPrice.Equal(decimal.NewFromInt(71500)) {
		t.Fatalf("exit_price=%s want 71500", trade.ExitPrice)
	}
	// Audit row created for the monitor trigger
	var audits []models.AuditLog
	exec.db.Where("symbol = ?", "BTCUSDT").Find(&audits)
	if len(audits) != 1 {
		t.Fatalf("audit_logs=%d want 1 (monitor row)", len(audits))
	}
	if audits[0].Action != "SELL" {
		t.Fatalf("audit_log.action=%s want SELL", audits[0].Action)
	}
	if audits[0].TradeID == nil || *audits[0].TradeID != trade.ID {
		t.Fatalf("audit_log.trade_id=%v want %d", audits[0].TradeID, trade.ID)
	}
}

func TestMonitor_StopLossClosesPosition(t *testing.T) {
	exec, mon, store := newMonitorHarness(t)
	seedOpenPosition(t, exec, "ETHUSDT",
		decimal.NewFromInt(3000), decimal.NewFromInt(1),
		decimal.NewFromInt(2900), decimal.NewFromInt(3200))

	store.Set("ETHUSDT", decimal.NewFromInt(2850), time.Now())
	mon.evaluateOnce(context.Background())

	var trade models.Trade
	if err := exec.db.Where("symbol = ? AND status = ?", "ETHUSDT", models.TradeStatusClosed).First(&trade).Error; err != nil {
		t.Fatalf("expected CLOSED trade: %v", err)
	}
	if trade.ExitReason != models.ExitReasonStopLoss {
		t.Fatalf("exit_reason=%s want STOP_LOSS", trade.ExitReason)
	}
	// Negative PnL — sold below entry.
	if !trade.PnL.IsNegative() {
		t.Fatalf("pnl=%s want negative", trade.PnL)
	}
}

func TestMonitor_NoSnapshot_NoOp(t *testing.T) {
	exec, mon, store := newMonitorHarness(t)
	seedOpenPosition(t, exec, "BTCUSDT",
		decimal.NewFromInt(70000), decimal.RequireFromString("0.001"),
		decimal.Zero, decimal.Zero) // both nil

	store.Set("BTCUSDT", decimal.NewFromInt(50000), time.Now()) // way below entry
	mon.evaluateOnce(context.Background())

	var pos models.Position
	exec.db.Where("symbol = ?", "BTCUSDT").First(&pos)
	if !pos.IsOpen {
		t.Fatal("position closed without an SL/TP snapshot")
	}
}

func TestMonitor_StalePrice_NoOp(t *testing.T) {
	exec, mon, store := newMonitorHarness(t)
	seedOpenPosition(t, exec, "BTCUSDT",
		decimal.NewFromInt(70000), decimal.RequireFromString("0.001"),
		decimal.NewFromInt(68000), decimal.NewFromInt(71000))

	// Quote 10 minutes old — older than the harness's 5min maxAge.
	store.Set("BTCUSDT", decimal.NewFromInt(72000), time.Now().Add(-10*time.Minute))
	mon.evaluateOnce(context.Background())

	var pos models.Position
	exec.db.Where("symbol = ?", "BTCUSDT").First(&pos)
	if !pos.IsOpen {
		t.Fatal("position closed on stale quote — monitor must refuse")
	}
}

func TestMonitor_NoQuote_NoOp(t *testing.T) {
	exec, mon, _ := newMonitorHarness(t)
	seedOpenPosition(t, exec, "BTCUSDT",
		decimal.NewFromInt(70000), decimal.RequireFromString("0.001"),
		decimal.NewFromInt(68000), decimal.NewFromInt(71000))

	mon.evaluateOnce(context.Background())

	var pos models.Position
	exec.db.Where("symbol = ?", "BTCUSDT").First(&pos)
	if !pos.IsOpen {
		t.Fatal("position closed without any quote ever set")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Atomic close — concurrent close attempts produce exactly one CLOSED
// ─────────────────────────────────────────────────────────────────────

func TestClosePosition_CAS_ExactlyOneWinner(t *testing.T) {
	exec, _, _ := newMonitorHarness(t)
	seedOpenPosition(t, exec, "BTCUSDT",
		decimal.NewFromInt(70000), decimal.RequireFromString("0.001"),
		decimal.NewFromInt(68000), decimal.NewFromInt(71000))

	const goroutines = 8
	type result struct {
		tradeID uint
		err     error
	}
	results := make([]result, goroutines)

	// Seed an audit log per caller so linkAuditLog succeeds.
	for i := range goroutines {
		entry := models.AuditLog{
			Symbol: "BTCUSDT", Interval: "1h", Prompt: "p", RawResponse: "r",
			Action: "SELL", PromptSentAt: time.Now(), ResponseAt: time.Now(),
		}
		exec.db.Create(&entry)
		results[i].tradeID = entry.ID // placeholder, overwritten below
	}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		auditID := results[i].tradeID
		go func(idx int) {
			defer wg.Done()
			id, err := exec.closePosition(context.Background(), closeFillInput{
				Symbol:     "BTCUSDT",
				ExitPrice:  decimal.NewFromInt(71500),
				ExitQty:    decimal.RequireFromString("0.001"),
				ExitFee:    decimal.Zero,
				ExitReason: models.ExitReasonTakeProfit,
				AuditLogID: auditID,
				IsPaper:    true,
			})
			results[idx] = result{tradeID: id, err: err}
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, r := range results {
		switch r.err {
		case nil:
			wins++
		case ErrAlreadyClosed:
			// expected loser
		default:
			t.Fatalf("unexpected error: %v", r.err)
		}
	}
	if wins != 1 {
		t.Fatalf("CAS winners=%d want exactly 1", wins)
	}
	// And the database state matches: exactly one CLOSED trade, position IS_OPEN=false.
	var closed []models.Trade
	exec.db.Where("symbol = ? AND status = ?", "BTCUSDT", models.TradeStatusClosed).Find(&closed)
	if len(closed) != 1 {
		t.Fatalf("CLOSED trades=%d want 1", len(closed))
	}
	var pos models.Position
	exec.db.Where("symbol = ?", "BTCUSDT").First(&pos)
	if pos.IsOpen {
		t.Fatal("position.is_open=true after winning close")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Lifecycle — bot can complete OPEN → CLOSE → OPEN repeatedly
// ─────────────────────────────────────────────────────────────────────

func TestLifecycle_OpenCloseOpenAgain(t *testing.T) {
	exec, mon, store := newMonitorHarness(t)
	// First cycle.
	seedOpenPosition(t, exec, "BTCUSDT",
		decimal.NewFromInt(70000), decimal.RequireFromString("0.001"),
		decimal.NewFromInt(68000), decimal.NewFromInt(71000))
	store.Set("BTCUSDT", decimal.NewFromInt(71500), time.Now())
	mon.evaluateOnce(context.Background())

	// Verify CLOSED.
	var first models.Position
	exec.db.Where("symbol = ?", "BTCUSDT").Order("id DESC").First(&first)
	if first.IsOpen {
		t.Fatal("first position not closed")
	}

	// Second cycle — new position on the same symbol.
	seedOpenPosition(t, exec, "BTCUSDT",
		decimal.NewFromInt(72000), decimal.RequireFromString("0.001"),
		decimal.NewFromInt(70000), decimal.NewFromInt(74000))
	store.Set("BTCUSDT", decimal.NewFromInt(69500), time.Now())
	mon.evaluateOnce(context.Background())

	// Now there should be TWO closed positions and TWO closed trades.
	var allPositions []models.Position
	exec.db.Where("symbol = ?", "BTCUSDT").Find(&allPositions)
	if len(allPositions) != 2 {
		t.Fatalf("positions=%d want 2", len(allPositions))
	}
	for _, p := range allPositions {
		if p.IsOpen {
			t.Fatalf("position %d still open", p.ID)
		}
	}
	var closedTrades []models.Trade
	exec.db.Where("symbol = ? AND status = ?", "BTCUSDT", models.TradeStatusClosed).Find(&closedTrades)
	if len(closedTrades) != 2 {
		t.Fatalf("closed trades=%d want 2", len(closedTrades))
	}
	// Reasons should be TP then SL.
	reasons := []models.ExitReason{closedTrades[0].ExitReason, closedTrades[1].ExitReason}
	hasTP, hasSL := false, false
	for _, r := range reasons {
		switch r {
		case models.ExitReasonTakeProfit:
			hasTP = true
		case models.ExitReasonStopLoss:
			hasSL = true
		}
	}
	if !hasTP || !hasSL {
		t.Fatalf("expected one TP + one SL across closed trades, got %v", reasons)
	}
}
