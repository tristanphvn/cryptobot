package ai

import (
	"strings"
	"testing"
)

func TestBuildPrompt_NoPosition_HasOnlyIndicators(t *testing.T) {
	mkt := MarketContext{Symbol: "BTCUSDT", CurrentPrice: 70000, RSI: 55}
	p := buildPrompt(mkt)
	if strings.Contains(p, "You currently hold an open position") {
		t.Fatal("position section leaked into entry-only prompt")
	}
	if !strings.Contains(p, "Market Data for BTCUSDT") {
		t.Fatal("base market section missing")
	}
}

func TestBuildPrompt_WithPosition_AppendsDiscretionarySection(t *testing.T) {
	mkt := MarketContext{
		Symbol: "BTCUSDT", CurrentPrice: 71500, RSI: 62,
		Position: &PositionContext{
			EntryPrice:       70000,
			Quantity:         0.001,
			UnrealizedPnLPct: 2.14,
			HoursHeld:        3.5,
			StopLoss:         68000,
			TakeProfit:       72000,
		},
	}
	p := buildPrompt(mkt)
	if !strings.Contains(p, "You currently hold an open position") {
		t.Fatal("position header missing")
	}
	if !strings.Contains(p, "$70000.00") {
		t.Fatal("entry price missing in rendered prompt")
	}
	if !strings.Contains(p, "SL=$68000.00") || !strings.Contains(p, "TP=$72000.00") {
		t.Fatal("SL/TP snapshot missing")
	}
	// Architectural assertion: discretionary wording, NOT bearish-anchored
	if strings.Contains(p, "technicals turn bearish") {
		t.Fatal("prompt must not hard-anchor model behavior with 'technicals turn bearish'")
	}
	if !strings.Contains(p, "Evaluate whether maintaining, exiting, or reversing") {
		t.Fatal("discretionary wording missing")
	}
}

func TestBuildPrompt_PositionWithoutSnapshot_OmitsRiskPlanLine(t *testing.T) {
	mkt := MarketContext{
		Symbol: "ETHUSDT", CurrentPrice: 3000,
		Position: &PositionContext{
			EntryPrice: 2900, Quantity: 1, UnrealizedPnLPct: 3.45, HoursHeld: 1,
		},
	}
	p := buildPrompt(mkt)
	if strings.Contains(p, "Risk plan at OPEN") {
		t.Fatal("risk plan line rendered without an actual snapshot")
	}
}

// CRY-22: peak / drawdown / armed lines render only when the data is
// present. Skipping them on zero values keeps the entry-only and
// pre-CRY-22 (backfill-pending) prompts unchanged from before.
func TestBuildPrompt_PositionWithPeakAndDrawdown_RendersTrailingLines(t *testing.T) {
	mkt := MarketContext{
		Symbol: "BTCUSDT", CurrentPrice: 72000, RSI: 58,
		Position: &PositionContext{
			EntryPrice:          70000,
			Quantity:            0.001,
			UnrealizedPnLPct:    2.86,
			HoursHeld:           5.2,
			PeakPrice:           74000,
			MaxUnrealizedPnLPct: 5.71,
			DrawdownFromPeakPct: 2.70,
			TrailingStopArmed:   true,
		},
	}
	p := buildPrompt(mkt)
	if !strings.Contains(p, "Peak Price Since Open: $74000.00") {
		t.Fatal("peak price line missing")
	}
	if !strings.Contains(p, "max unrealized +5.71%") {
		t.Fatal("max unrealized rendering wrong")
	}
	if !strings.Contains(p, "current drawdown from peak 2.70%") {
		t.Fatal("drawdown rendering wrong")
	}
	if !strings.Contains(p, "Trailing stop is ARMED") {
		t.Fatal("armed line missing")
	}
}

func TestBuildPrompt_PositionNoPeak_OmitsTrailingLines(t *testing.T) {
	// Pre-CRY-22 row or fresh position before monitor's first tick —
	// PeakPrice = 0 should cleanly omit the peak/armed block.
	mkt := MarketContext{
		Symbol: "BTCUSDT", CurrentPrice: 71000,
		Position: &PositionContext{
			EntryPrice:       70000,
			Quantity:         0.001,
			UnrealizedPnLPct: 1.43,
			HoursHeld:        0.5,
		},
	}
	p := buildPrompt(mkt)
	if strings.Contains(p, "Peak Price Since Open") {
		t.Fatal("peak line leaked when PeakPrice=0")
	}
	if strings.Contains(p, "Trailing stop is ARMED") {
		t.Fatal("armed line leaked when TrailingStopArmed=false")
	}
}
