package riskmanager

import "testing"

func TestMinExpectedReturnGate(t *testing.T) {
	tests := []struct {
		name        string
		takeProfit  float64
		price       float64
		capBps      int64
		wantBlocked bool
		wantBps     int64
	}{
		{
			name:        "well above cap → allowed",
			takeProfit:  102.0, // +200 bps
			price:       100.0,
			capBps:      30,
			wantBlocked: false,
			wantBps:     200,
		},
		{
			name:        "exactly at cap → allowed (no rounding-down rejection)",
			takeProfit:  100.3, // floats land at 29.999... but math.Round → 30
			price:       100.0,
			capBps:      30,
			wantBlocked: false,
			wantBps:     30,
		},
		{
			name:        "just below cap → blocked",
			takeProfit:  100.29, // 29 bps after rounding
			price:       100.0,
			capBps:      30,
			wantBlocked: true,
			wantBps:     29,
		},
		{
			name:        "TP below price → blocked, bps=0",
			takeProfit:  99.0,
			price:       100.0,
			capBps:      30,
			wantBlocked: true,
			wantBps:     0,
		},
		{
			name:        "TP equal to price → blocked",
			takeProfit:  100.0,
			price:       100.0,
			capBps:      30,
			wantBlocked: true,
			wantBps:     0,
		},
		{
			name:        "zero price → blocked",
			takeProfit:  100.0,
			price:       0,
			capBps:      30,
			wantBlocked: true,
			wantBps:     0,
		},
		{
			name:        "zero takeProfit → blocked",
			takeProfit:  0,
			price:       100.0,
			capBps:      30,
			wantBlocked: true,
			wantBps:     0,
		},
		{
			name:        "cap=0 disables the gate even with zero-upside signal",
			takeProfit:  100.0,
			price:       100.0,
			capBps:      0,
			wantBlocked: false,
			wantBps:     0,
		},
		{
			name:        "negative cap also disables",
			takeProfit:  50.0,
			price:       100.0,
			capBps:      -1,
			wantBlocked: false,
			wantBps:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocked, bps := MinExpectedReturnGate(tt.takeProfit, tt.price, tt.capBps)
			if blocked != tt.wantBlocked {
				t.Errorf("blocked: got %v want %v", blocked, tt.wantBlocked)
			}
			if bps != tt.wantBps {
				t.Errorf("bps: got %d want %d", bps, tt.wantBps)
			}
		})
	}
}
