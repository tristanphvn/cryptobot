package riskmanager

import (
	"errors"
	"testing"
	"time"

	"cryptobot/internal/database/models"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func seedClosedTrade(t *testing.T, db *gorm.DB, gross, net int64, exitAt time.Time) {
	t.Helper()
	if err := db.Create(&models.Trade{
		Symbol: "BTCUSDT", Side: models.TradeSideBuy, Status: models.TradeStatusClosed,
		PnL: decimal.NewFromInt(gross), NetPnL: decimal.NewFromInt(net),
		EntryAt: exitAt, ExitAt: &exitAt,
	}).Error; err != nil {
		t.Fatalf("seed trade: %v", err)
	}
}

// CRY-32: fees count toward the daily loss. 20 trades at gross -22 /
// net -27 → net loss 540 = 5.4% of start-of-day equity 10 000. Gross
// alone (440) would stay under the 5% cap.
func TestDailyDrawdown_UsesNetPnL(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC()
	for i := 0; i < 20; i++ {
		seedClosedTrade(t, db, -22, -27, now)
	}
	rm := New(db, Config{MaxPositionPct: 2, DailyDrawdownPct: 5, MaxOpenPositions: 3})

	err := rm.PreTradeCheck("BTCUSDT", models.TradeSideBuy, rm.Equity())
	var breach Breach
	if !errors.As(err, &breach) || breach.Rule != "daily_drawdown" {
		t.Fatalf("err=%v want daily_drawdown breach", err)
	}
}

// Loss is measured against start-of-day equity, not post-loss equity:
// 480 lost from 10 000 is 4.8% (under 5%), even though 480/9 520 = 5.04%.
func TestDailyDrawdown_StartOfDayDenominator(t *testing.T) {
	db := newTestDB(t)
	seedClosedTrade(t, db, -470, -480, time.Now().UTC())
	rm := New(db, Config{MaxPositionPct: 2, DailyDrawdownPct: 5, MaxOpenPositions: 3})

	if err := rm.PreTradeCheck("BTCUSDT", models.TradeSideBuy, rm.Equity()); err != nil {
		t.Fatalf("err=%v want nil (4.8%% of start-of-day equity)", err)
	}
	if got := rm.GetStatus().DailyDrawdownPct; got < 4.79 || got > 4.81 {
		t.Fatalf("daily_drawdown_pct=%v want 4.8", got)
	}
}

// Previous days' losses don't count toward today, but do lower equity.
func TestDailyDrawdown_IgnoresPreviousDays(t *testing.T) {
	db := newTestDB(t)
	seedClosedTrade(t, db, -900, -1000, time.Now().UTC().AddDate(0, 0, -2))
	seedClosedTrade(t, db, -300, -360, time.Now().UTC())
	rm := New(db, Config{MaxPositionPct: 2, DailyDrawdownPct: 5, MaxOpenPositions: 3})

	if !rm.Equity().Equal(decimal.NewFromInt(8640)) {
		t.Fatalf("equity=%s want 8640", rm.Equity())
	}
	// 360 / (8640 + 360) = 4%.
	if got := rm.GetStatus().DailyDrawdownPct; got < 3.99 || got > 4.01 {
		t.Fatalf("daily_drawdown_pct=%v want 4", got)
	}
}

func TestGetStatus_DailyFiguresAreNetAndPercent(t *testing.T) {
	db := newTestDB(t)
	seedClosedTrade(t, db, -300, -310, time.Now().UTC())
	rm := New(db, Config{MaxPositionPct: 2, DailyDrawdownPct: 5, MaxOpenPositions: 3})

	st := rm.GetStatus()
	if !st.DailyPnL.Equal(decimal.NewFromInt(-310)) {
		t.Fatalf("daily_pnl=%s want -310 (net)", st.DailyPnL)
	}
	if st.DailyDrawdownPct < 3.09 || st.DailyDrawdownPct > 3.11 {
		t.Fatalf("daily_drawdown_pct=%v want 3.1 (percent, not USDT)", st.DailyDrawdownPct)
	}
}

func TestGetStatus_ProfitableDayIsZeroDrawdown(t *testing.T) {
	db := newTestDB(t)
	seedClosedTrade(t, db, 120, 100, time.Now().UTC())
	rm := New(db, Config{MaxPositionPct: 2, DailyDrawdownPct: 5, MaxOpenPositions: 3})
	if got := rm.GetStatus().DailyDrawdownPct; got != 0 {
		t.Fatalf("daily_drawdown_pct=%v want 0", got)
	}
}

func TestCircuitBreaker_TripsAndAutoResetsNextDay(t *testing.T) {
	db := newTestDB(t)
	seedClosedTrade(t, db, -550, -600, time.Now().UTC())
	rm := New(db, Config{MaxPositionPct: 2, DailyDrawdownPct: 5, MaxOpenPositions: 3})

	rm.PostTradeCheck(rm.Equity())
	if !rm.IsHalted() {
		t.Fatal("breaker should trip at 6% daily loss")
	}

	// Simulate the trip having happened yesterday.
	rm.mu.Lock()
	rm.haltedDay = todayUTC().AddDate(0, 0, -1)
	rm.mu.Unlock()
	if rm.IsHalted() {
		t.Fatal("breaker should auto-reset on a new UTC day")
	}
}

func TestEquity_BaseCapitalOverride(t *testing.T) {
	db := newTestDB(t)
	seedClosedTrade(t, db, 60, 50, time.Now().UTC())
	rm := New(db, Config{BaseCapital: decimal.NewFromInt(500)})
	if !rm.Equity().Equal(decimal.NewFromInt(550)) {
		t.Fatalf("equity=%s want 550", rm.Equity())
	}
}

// seedClosedTradeSym inserts a CLOSED trade dated now (inside today's UTC window
// the drawdown query scans) with distinct gross pnl and net_pnl so a test can
// prove the breaker reads net_pnl, not gross pnl.
func seedClosedTradeSym(t *testing.T, db *gorm.DB, symbol, pnl, netPnL string) {
	t.Helper()
	now := time.Now().UTC()
	if err := db.Create(&models.Trade{
		Symbol:     symbol,
		Side:       models.TradeSideBuy,
		Status:     models.TradeStatusClosed,
		EntryPrice: decimal.NewFromInt(100),
		Quantity:   decimal.NewFromInt(1),
		PnL:        decimal.RequireFromString(pnl),
		NetPnL:     decimal.RequireFromString(netPnL),
		EntryAt:    now,
		ExitAt:     &now,
	}).Error; err != nil {
		t.Fatalf("seed closed trade %s: %v", symbol, err)
	}
}

// RISK-1: the daily-drawdown breaker must sum net_pnl (after fees), not gross
// pnl. The equity denominator (getEquity) is net of fees, so mixing a gross
// numerator under-counts the real wallet loss and trips the breaker too late.
//
// This seeds a day whose GROSS pnl is POSITIVE (+3) but whose NET pnl is a LOSS
// (-1) once fees are counted. A gross-based breaker sees no drawdown; the
// net-based breaker must trip. Reverting SUM(net_pnl) back to SUM(pnl) in
// checkDailyDrawdown or GetStatus makes this test fail — it is the regression
// guard the review flagged as missing.
func TestCheckDailyDrawdown_UsesNetPnLNotGross(t *testing.T) {
	db := newTestDB(t)
	// gross: +5 + -2 = +3 (positive, no drawdown)
	// net:   +2 + -3 = -1 (loss after fees)
	seedClosedTradeSym(t, db, "BTCUSDT", "5", "2")
	seedClosedTradeSym(t, db, "ETHUSDT", "-2", "-3")

	equity := decimal.NewFromInt(10000)
	// |-1| / 10000 = 0.01% loss. Threshold 0.005% => trips only on the net
	// figure; the gross +3 would never trip.
	rm := New(db, Config{DailyDrawdownPct: 0.005, MaxOpenPositions: 3})

	// GetStatus must report the NET daily pnl (-1), not the gross (+3).
	if got := rm.GetStatus().DailyPnL; !got.Equal(decimal.RequireFromString("-1")) {
		t.Fatalf("GetStatus().DailyPnL=%s want -1 (net_pnl sum, not gross +3)", got)
	}

	// checkDailyDrawdown must return a daily_drawdown breach on the net loss.
	err := rm.checkDailyDrawdown(equity)
	var b Breach
	if !errors.As(err, &b) || b.Rule != "daily_drawdown" {
		t.Fatalf("expected daily_drawdown breach on net loss, got %v", err)
	}

	// Full path: PostTradeCheck trips and latches the circuit breaker.
	rm.PostTradeCheck(equity)
	if !rm.GetStatus().Halted {
		t.Fatal("expected circuit breaker halted after net drawdown")
	}
}

// A day that is a NET profit must NOT trip the breaker even if a single gross
// leg is negative — guards against an over-eager breaker after the column swap.
func TestCheckDailyDrawdown_NetProfitDoesNotTrip(t *testing.T) {
	db := newTestDB(t)
	// net: +10 + -1 = +9 (profit) => no drawdown regardless of gross.
	seedClosedTradeSym(t, db, "BTCUSDT", "12", "10")
	seedClosedTradeSym(t, db, "ETHUSDT", "1", "-1")

	rm := New(db, Config{DailyDrawdownPct: 0.005, MaxOpenPositions: 3})
	if err := rm.checkDailyDrawdown(decimal.NewFromInt(10000)); err != nil {
		t.Fatalf("expected no breach on net profit, got %v", err)
	}
	if got := rm.GetStatus().DailyPnL; !got.Equal(decimal.RequireFromString("9")) {
		t.Fatalf("GetStatus().DailyPnL=%s want 9 (net)", got)
	}
}
