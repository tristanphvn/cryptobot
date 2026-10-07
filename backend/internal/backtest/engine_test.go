package backtest

import (
	"math"
	"testing"

	"github.com/shopspring/decimal"
)

func mkTrade(net float64) TradeRecord {
	return TradeRecord{
		NetPnL:    decimal.NewFromFloat(net),
		NetPnLPct: decimal.NewFromFloat(net), // for simplicity, treat as percent
	}
}

func TestLongestLosingStreak(t *testing.T) {
	tests := []struct {
		name string
		nets []float64
		want int
	}{
		{"empty", nil, 0},
		{"all wins", []float64{1, 2, 3}, 0},
		{"all losses", []float64{-1, -2, -3}, 3},
		{"single loss surrounded by wins", []float64{1, -1, 1}, 1},
		{"two streaks pick longest", []float64{-1, -1, 1, -1, -1, -1, 1}, 3},
		{"zeros break the streak", []float64{-1, -1, 0, -1}, 2},
		{"trailing loss", []float64{1, -1, -1, -1}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trades := make([]TradeRecord, len(tt.nets))
			for i, n := range tt.nets {
				trades[i] = mkTrade(n)
			}
			got := longestLosingStreak(trades)
			if got != tt.want {
				t.Errorf("got %d want %d", got, tt.want)
			}
		})
	}
}

func TestComputeSortino_OnlyDownsidePunishes(t *testing.T) {
	// Series with a big winner and several small losers: Sharpe should be
	// modest (volatility includes the winner); Sortino should be higher
	// (downside stdev ignores the winner, denominator shrinks).
	trades := []TradeRecord{
		mkTrade(10), // big win — drives mean up
		mkTrade(-1), mkTrade(-1), mkTrade(-1),
		mkTrade(2),
	}
	sharpe := computeSharpe(trades)
	sortino := computeSortino(trades)
	if sortino <= sharpe {
		t.Errorf("expected Sortino > Sharpe for asymmetric series: sortino=%.4f sharpe=%.4f", sortino, sharpe)
	}
}

func TestComputeSortino_NoLosses(t *testing.T) {
	trades := []TradeRecord{mkTrade(1), mkTrade(2), mkTrade(3)}
	if got := computeSortino(trades); got != 0 {
		t.Errorf("Sortino with no losses must be 0 (undefined); got %.4f", got)
	}
}

func TestComputeSharpe_TooFewTrades(t *testing.T) {
	if got := computeSharpe(nil); got != 0 {
		t.Errorf("Sharpe of empty slice: got %.4f want 0", got)
	}
	if got := computeSharpe([]TradeRecord{mkTrade(1)}); got != 0 {
		t.Errorf("Sharpe with single trade: got %.4f want 0", got)
	}
}

func TestVerdict_PassesWhenAllGatesMet(t *testing.T) {
	r := &Result{
		WinRate:        60.0,
		MaxDrawdownPct: decimal.NewFromFloat(10),
		SharpeRatio:    1.2,
		NetPnL:         decimal.NewFromFloat(500),
	}
	v := verdictFor(r)
	if !v.Pass {
		t.Errorf("expected PASS; reasons=%v", v.Reasons)
	}
}

func TestVerdict_FailEnumeratesEveryRule(t *testing.T) {
	// Every threshold breached so we know the function doesn't short-
	// circuit on the first failure.
	r := &Result{
		WinRate:        40,                              // < 55
		MaxDrawdownPct: decimal.NewFromFloat(20),        // > 15
		SharpeRatio:    0.5,                             // < 1.0
		NetPnL:         decimal.NewFromFloat(-100),      // ≤ 0
	}
	v := verdictFor(r)
	if v.Pass {
		t.Fatal("expected FAIL")
	}
	if len(v.Reasons) != 4 {
		t.Errorf("expected 4 reasons, got %d: %v", len(v.Reasons), v.Reasons)
	}
}

func TestCacheStats_HitRate(t *testing.T) {
	tests := []struct {
		name string
		cs   CacheStats
		want float64
	}{
		{"empty", CacheStats{}, 0},
		{"only hits", CacheStats{Hits: 10}, 1.0},
		{"only misses", CacheStats{Misses: 5}, 0.0},
		{"95%", CacheStats{Hits: 95, Misses: 5}, 0.95},
		{"skipped not counted", CacheStats{Hits: 1, Misses: 1, Skipped: 99}, 0.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cs.HitRate()
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("got %.4f want %.4f", got, tt.want)
			}
		})
	}
}
