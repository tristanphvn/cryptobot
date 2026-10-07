package backtest

import (
	"testing"

	"cryptobot/internal/database/models"

	"github.com/shopspring/decimal"
)

// fptr returns a *decimal.Decimal for an SL/TP level, or nil for "no level".
func fptr(f float64) *decimal.Decimal {
	d := decimal.NewFromFloat(f)
	return &d
}

func cndl(o, h, l, c float64) models.Candle {
	return models.Candle{
		Open:  decimal.NewFromFloat(o),
		High:  decimal.NewFromFloat(h),
		Low:   decimal.NewFromFloat(l),
		Close: decimal.NewFromFloat(c),
	}
}

// TestSimulateDeterministicExit pins the PROF-1 fix: the backtest must replay
// the live monitor's STOP_LOSS > TRAILING_STOP > TAKE_PROFIT hierarchy on
// every candle, with realistic intrabar fills and adverse slippage.
func TestSimulateDeterministicExit(t *testing.T) {
	feeMul := decimal.NewFromFloat(0.00075) // 7.5 bps per leg

	cases := []struct {
		name                           string
		entry, peak                    float64
		armed                          bool
		sl, tp                         *decimal.Decimal
		candle                         models.Candle
		slipBps                        float64
		trailPct, armPct, minLockedPct float64
		wantFired                      bool
		wantReason                     models.ExitReason
		wantExit                       float64
	}{
		{
			name: "stop-loss hit fills at the stop level",
			entry: 100, peak: 100, sl: fptr(95), tp: fptr(110),
			candle:    cndl(100, 102, 94, 96), // low 94 <= SL 95
			wantFired: true, wantReason: models.ExitReasonStopLoss, wantExit: 95,
		},
		{
			name: "gap-down through stop fills at the open",
			entry: 100, peak: 100, sl: fptr(95),
			candle:    cndl(93, 94, 90, 91), // opened below the stop
			wantFired: true, wantReason: models.ExitReasonStopLoss, wantExit: 93,
		},
		{
			name: "take-profit hit fills at the target",
			entry: 100, peak: 100, tp: fptr(110),
			candle:    cndl(105, 112, 104, 111), // high 112 >= TP 110
			wantFired: true, wantReason: models.ExitReasonTakeProfit, wantExit: 110,
		},
		{
			name: "gap-up through target fills at the better open",
			entry: 100, peak: 100, tp: fptr(110),
			candle:    cndl(115, 118, 114, 116),
			wantFired: true, wantReason: models.ExitReasonTakeProfit, wantExit: 115,
		},
		{
			name: "stop-loss wins when one candle spans both stop and target",
			entry: 100, peak: 100, sl: fptr(95), tp: fptr(110),
			candle:    cndl(100, 111, 94, 100), // both touched intrabar
			wantFired: true, wantReason: models.ExitReasonStopLoss, wantExit: 95,
		},
		{
			name: "no level touched -> no exit",
			entry: 100, peak: 100, sl: fptr(95), tp: fptr(110),
			candle:    cndl(100, 108, 97, 105),
			wantFired: false,
		},
		{
			name: "stop-loss takes adverse slippage on the fill",
			entry: 100, peak: 100, sl: fptr(95),
			candle:  cndl(100, 101, 94, 96),
			slipBps: 10, // 0.1% -> 95 * 0.999
			wantFired: true, wantReason: models.ExitReasonStopLoss, wantExit: 94.905,
		},
		{
			name: "trailing disabled never fires even when armed-by-peak",
			entry: 100, peak: 110, sl: fptr(80), tp: fptr(200),
			candle:    cndl(106, 106, 100, 101), // deep retrace from peak
			trailPct:  0,                         // disabled
			wantFired: false,
		},
		{
			name: "trailing fires once armed and price crosses the trail",
			entry: 100, peak: 110, sl: fptr(80), tp: fptr(200),
			candle:                            cndl(106, 106, 104, 104),
			trailPct: 5, armPct: 5, minLockedPct: 0,
			// threshold = 110 * (1 - 0.05) = 104.5; low 104 <= 104.5
			wantFired: true, wantReason: models.ExitReasonTrailingStop, wantExit: 104.5,
		},
		{
			name: "trailing held back by min-locked-profit gate",
			entry: 100, peak: 110, sl: fptr(80), tp: fptr(200),
			candle:                            cndl(106, 106, 104, 104),
			trailPct: 5, armPct: 5, minLockedPct: 10, // require 10% net; ~4.35% locked
			wantFired: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pos := &btPosition{
				entryPrice: decimal.NewFromFloat(tc.entry),
				entryQty:   decimal.NewFromInt(1),
				peak:       decimal.NewFromFloat(tc.peak),
				trailArmed: tc.armed,
				sl:         tc.sl,
				tp:         tc.tp,
			}
			slipMul := decimal.NewFromFloat(tc.slipBps).Div(decimal.NewFromFloat(10000))

			exit, reason, fired := simulateDeterministicExit(
				tc.candle, pos, slipMul, feeMul,
				tc.trailPct, tc.armPct, tc.minLockedPct,
			)

			if fired != tc.wantFired {
				t.Fatalf("fired = %v, want %v (reason=%q exit=%s)", fired, tc.wantFired, reason, exit)
			}
			if !fired {
				return
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			got := exit.InexactFloat64()
			if diff := got - tc.wantExit; diff > 1e-6 || diff < -1e-6 {
				t.Errorf("exit = %.6f, want %.6f", got, tc.wantExit)
			}
		})
	}
}

// TestSimulateDeterministicExit_PeakRatchets confirms the running peak is
// advanced by the candle high (needed for trailing on later bars).
func TestSimulateDeterministicExit_PeakRatchets(t *testing.T) {
	pos := &btPosition{
		entryPrice: decimal.NewFromFloat(100),
		entryQty:   decimal.NewFromInt(1),
		peak:       decimal.NewFromFloat(100),
	}
	// No SL/TP, trailing disabled — only the peak update should happen.
	_, _, fired := simulateDeterministicExit(cndl(101, 115, 100, 112), pos,
		decimal.Zero, decimal.NewFromFloat(0.00075), 0, 0, 0)
	if fired {
		t.Fatal("did not expect an exit with no levels and trailing off")
	}
	if !pos.peak.Equal(decimal.NewFromFloat(115)) {
		t.Errorf("peak = %s, want 115 (should ratchet to candle high)", pos.peak)
	}
}

func TestSlippage(t *testing.T) {
	slip := decimal.NewFromFloat(10).Div(decimal.NewFromFloat(10000)) // 10 bps
	price := decimal.NewFromFloat(100)
	if got := slippedBuy(price, slip).InexactFloat64(); got != 100.1 {
		t.Errorf("slippedBuy = %.4f, want 100.1 (fills higher)", got)
	}
	if got := slippedSell(price, slip).InexactFloat64(); got != 99.9 {
		t.Errorf("slippedSell = %.4f, want 99.9 (fills lower)", got)
	}
	// Zero slippage is a no-op.
	if !slippedBuy(price, decimal.Zero).Equal(price) || !slippedSell(price, decimal.Zero).Equal(price) {
		t.Error("zero slippage must not move the fill price")
	}
}

func TestNetLockedPnLPctBT(t *testing.T) {
	// entry 100, exit 105, 7.5 bps/leg: gross 5%, fees ~0.154 -> ~4.85% net.
	got := netLockedPnLPctBT(decimal.NewFromFloat(100), decimal.NewFromFloat(105),
		decimal.NewFromFloat(0.00075)).InexactFloat64()
	if got < 4.8 || got > 4.9 {
		t.Errorf("net locked pct = %.4f, want ~4.85", got)
	}
	if got := netLockedPnLPctBT(decimal.Zero, decimal.NewFromFloat(105), decimal.NewFromFloat(0.00075)); !got.IsZero() {
		t.Errorf("zero entry must return 0, got %s", got)
	}
}
