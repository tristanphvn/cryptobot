package riskmanager

import "testing"

// CRY-37: reject a BUY whose stop sits more than capPct below the price.
func TestMaxStopLossGate(t *testing.T) {
	cases := []struct {
		name              string
		sl, price, capPct float64
		wantBlock         bool
	}{
		{"inside cap", 97, 100, 5, false},
		{"exactly at cap admitted", 95, 100, 5, false},
		{"just past cap", 94.99, 100, 5, true},
		{"DOGE #23 (-9.5%)", 0.0755, 0.08343, 5, true},
		{"sub-dollar exactly 5% admitted", 0.0950, 0.1, 5, false},
		{"no stop (AI declined) admitted", 0, 100, 5, false},
		{"gate disabled", 50, 100, 0, false},
		{"wrong-side stop left to its own gate", 105, 100, 5, false},
		{"no price fails closed", 95, 0, 5, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, dist := MaxStopLossGate(c.sl, c.price, c.capPct); got != c.wantBlock {
				t.Fatalf("blocked=%v (dist=%.4f%%) want %v", got, dist, c.wantBlock)
			}
		})
	}
}
