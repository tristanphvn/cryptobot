package executor

import (
	"context"
	"testing"

	"cryptobot/internal/ai"
	"cryptobot/internal/database/models"
	"cryptobot/internal/riskmanager"
	"cryptobot/internal/strategy"

	"gorm.io/gorm"
)

// newGateExecutor builds a paper-mode executor with the min-return gate OFF
// (minExpectedReturnBps defaults to 0) so a test isolates the STR-1/EXIT-1
// wrong-side gate — otherwise a bad TP could be caught by the min-return gate
// and mask a regression here.
func newGateExecutor(t *testing.T) (*Executor, *gorm.DB) {
	t.Helper()
	db := newIsolatedTestDB(t)
	risk := riskmanager.New(db, riskmanager.Config{
		MaxPositionPct: 2.0, DailyDrawdownPct: 5.0, MaxOpenPositions: 50,
	})
	return NewExecutor(db, nil, risk, ModePaper, nil), db
}

func evalBUY(symbol string, price, sl, tp float64) *strategy.EvalResult {
	return &strategy.EvalResult{
		AnalysisResult: &ai.AnalysisResult{
			Signal: &ai.Signal{Action: "BUY", Confidence: 0.9, StopLoss: sl, TakeProfit: tp},
		},
		Market: ai.MarketContext{Symbol: symbol, CurrentPrice: price},
	}
}

// STR-1 / EXIT-1: route() must reject a BUY whose AI stop-loss sits at/above
// the entry, or take-profit at/below it, because the monitor exits on
// price ≤ SL and price ≥ TP — such a level fires immediately for a fee-eaten
// loss. A zero/absent level (AI declined) must still be admitted.
func TestRoute_RejectsWrongSideSLTP(t *testing.T) {
	const price = 100.0
	cases := []struct {
		name      string
		sl, tp    float64
		wantBlock string // "" => should admit and open a trade
	}{
		{"valid long: SL below, TP above", 95, 110, ""},
		{"SL equal to entry stops out", 100, 110, "risk:invalid_stop_loss"},
		{"SL above entry stops out", 105, 110, "risk:invalid_stop_loss"},
		{"TP equal to entry takes profit", 95, 100, "risk:invalid_take_profit"},
		{"TP below entry takes profit", 95, 90, "risk:invalid_take_profit"},
		{"no SL/TP (AI declined) admitted", 0, 0, ""},
		{"only valid TP, no SL", 0, 110, ""},
		{"only valid SL, no TP", 95, 0, ""},
		{"invalid SL takes precedence over invalid TP", 105, 90, "risk:invalid_stop_loss"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exec, db := newGateExecutor(t)
			eval := evalBUY("BTCUSDT", price, c.sl, c.tp)

			tradeID, blockedBy, err := exec.route(context.Background(), "BTCUSDT", "1h", eval, 0)
			if err != nil {
				t.Fatalf("route err: %v", err)
			}

			var count int64
			db.Model(&models.Trade{}).Count(&count)

			if c.wantBlock == "" {
				if blockedBy != "" || tradeID == nil {
					t.Fatalf("expected admit, got blockedBy=%q tradeID=%v", blockedBy, tradeID)
				}
				if count != 1 {
					t.Fatalf("expected 1 trade row after admit, got %d", count)
				}
				return
			}
			if blockedBy != c.wantBlock {
				t.Fatalf("blockedBy=%q want %q", blockedBy, c.wantBlock)
			}
			if tradeID != nil {
				t.Fatalf("expected no trade on invalid signal, got id=%d", *tradeID)
			}
			if count != 0 {
				t.Fatalf("invalid signal leaked a trade: %d rows", count)
			}
		})
	}
}

// A SELL must never be screened by the wrong-side gate — it closes a position,
// not opens one, and its SL/TP fields are irrelevant to entry safety.
func TestRoute_SellBypassesWrongSideGate(t *testing.T) {
	exec, _ := newGateExecutor(t)
	eval := &strategy.EvalResult{
		AnalysisResult: &ai.AnalysisResult{
			// SL/TP would both be "wrong side" for a long, but this is a SELL.
			Signal: &ai.Signal{Action: "SELL", Confidence: 0.9, StopLoss: 105, TakeProfit: 90},
		},
		Market: ai.MarketContext{Symbol: "BTCUSDT", CurrentPrice: 100},
	}
	_, blockedBy, err := exec.route(context.Background(), "BTCUSDT", "1h", eval, 0)
	if err != nil {
		t.Fatalf("route err: %v", err)
	}
	// No open position exists, so a SELL is a no-op ("no_position"), but it must
	// NOT be blocked by the invalid-SL/TP gate.
	if blockedBy == "risk:invalid_stop_loss" || blockedBy == "risk:invalid_take_profit" {
		t.Fatalf("SELL wrongly screened by entry SL/TP gate: blockedBy=%q", blockedBy)
	}
}

// CRY-37: with the stop-distance cap on, route() rejects a BUY whose stop
// is wider than the cap, admits one at the cap, and leaves SELL alone.
func TestRoute_MaxStopLossGate(t *testing.T) {
	const price = 100.0
	cases := []struct {
		name      string
		capPct    float64
		sl        float64
		wantBlock string
	}{
		{"stop 3% admitted", 5, 97, ""},
		{"stop exactly 5% admitted", 5, 95, ""},
		{"stop 9.5% blocked", 5, 90.5, "risk:stop_loss_too_wide"},
		{"no stop admitted", 5, 0, ""},
		{"gate off admits wide stop", 0, 80, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exec, db := newGateExecutor(t)
			exec.WithMaxStopLossGate(c.capPct)
			tradeID, blockedBy, err := exec.route(context.Background(), "BTCUSDT", "1h", evalBUY("BTCUSDT", price, c.sl, 110), 0)
			if err != nil {
				t.Fatalf("route err: %v", err)
			}
			var count int64
			db.Model(&models.Trade{}).Count(&count)
			if blockedBy != c.wantBlock {
				t.Fatalf("blockedBy=%q want %q", blockedBy, c.wantBlock)
			}
			if c.wantBlock == "" && (tradeID == nil || count != 1) {
				t.Fatalf("expected admit with 1 trade, got tradeID=%v rows=%d", tradeID, count)
			}
			if c.wantBlock != "" && (tradeID != nil || count != 0) {
				t.Fatalf("blocked signal leaked a trade: tradeID=%v rows=%d", tradeID, count)
			}
		})
	}

	exec, _ := newGateExecutor(t)
	exec.WithMaxStopLossGate(5)
	sell := &strategy.EvalResult{
		AnalysisResult: &ai.AnalysisResult{Signal: &ai.Signal{Action: "SELL", Confidence: 0.9, StopLoss: 50}},
		Market:         ai.MarketContext{Symbol: "BTCUSDT", CurrentPrice: price},
	}
	if _, blockedBy, _ := exec.route(context.Background(), "BTCUSDT", "1h", sell, 0); blockedBy == "risk:stop_loss_too_wide" {
		t.Fatal("SELL must not be screened by the stop-distance gate")
	}
}
