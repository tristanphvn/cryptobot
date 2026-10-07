package executor

import (
	"context"
	"errors"
	"testing"

	"cryptobot/internal/database/models"
	"cryptobot/internal/execution"

	"github.com/shopspring/decimal"
)

func TestBaseAssetOf(t *testing.T) {
	cases := []struct{ sym, quote, want string }{
		{"BTCUSDT", "USDT", "BTC"},
		{"ETHUSDT", "USDT", "ETH"},
		{"USDT", "USDT", ""},   // nothing left after the quote suffix
		{"BTCUSD", "USDT", ""}, // doesn't end in the quote asset
		{"", "USDT", ""},
	}
	for _, c := range cases {
		if got := baseAssetOf(c.sym, c.quote); got != c.want {
			t.Errorf("baseAssetOf(%q,%q)=%q want %q", c.sym, c.quote, got, c.want)
		}
	}
}

func TestFeeFromFillsValued(t *testing.T) {
	dec := decimal.RequireFromString
	price := decimal.NewFromInt(70000)

	t.Run("quote commission taken as-is", func(t *testing.T) {
		fee, unv := feeFromFillsValued([]execution.Fill{
			{Commission: dec("0.026"), CommissionAsset: "USDT", Price: price},
			{Commission: dec("0.026"), CommissionAsset: "USDT", Price: price},
		}, "USDT", "BTC", nil)
		if unv || !fee.Equal(dec("0.052")) {
			t.Fatalf("fee=%s unvalued=%v want 0.052/false", fee, unv)
		}
	})

	t.Run("base commission valued at fill price", func(t *testing.T) {
		// 0.000001 BTC * 70000 = 0.07 USDT
		fee, unv := feeFromFillsValued([]execution.Fill{
			{Commission: dec("0.000001"), CommissionAsset: "BTC", Price: price},
		}, "USDT", "BTC", nil)
		if unv || !fee.Equal(dec("0.07")) {
			t.Fatalf("fee=%s unvalued=%v want 0.07/false", fee, unv)
		}
	})

	t.Run("unknown asset flagged unvalued and skipped", func(t *testing.T) {
		fee, unv := feeFromFillsValued([]execution.Fill{
			{Commission: dec("0.0001"), CommissionAsset: "BNB", Price: price},
		}, "USDT", "BTC", nil)
		if !unv || !fee.IsZero() {
			t.Fatalf("fee=%s unvalued=%v want 0/true (BNB skipped)", fee, unv)
		}
	})

	t.Run("mixed base + quote summed in quote terms", func(t *testing.T) {
		fee, unv := feeFromFillsValued([]execution.Fill{
			{Commission: dec("0.000001"), CommissionAsset: "BTC", Price: price}, // 0.07
			{Commission: dec("0.01"), CommissionAsset: "USDT", Price: price},    // 0.01
		}, "USDT", "BTC", nil)
		if unv || !fee.Equal(dec("0.08")) {
			t.Fatalf("fee=%s unvalued=%v want 0.08/false", fee, unv)
		}
	})
}

func TestBaseCommissionQty(t *testing.T) {
	dec := decimal.RequireFromString
	fills := []execution.Fill{
		{Commission: dec("0.000001"), CommissionAsset: "BTC"},
		{Commission: dec("0.000002"), CommissionAsset: "BTC"},
		{Commission: dec("0.05"), CommissionAsset: "USDT"},
	}
	if got := baseCommissionQty(fills, "BTC"); !got.Equal(dec("0.000003")) {
		t.Fatalf("baseCommissionQty=%s want 0.000003", got)
	}
	if got := baseCommissionQty(fills, ""); !got.IsZero() {
		t.Fatalf("empty base must return 0, got %s", got)
	}
}

// End-to-end: a Spot MARKET BUY commissioned in the base asset (BTC) must
// record the fee valued in USDT (EXP-1) and net the commission out of the
// stored quantity (EXP-2), so a later close SELL sizes against real holdings
// instead of being rejected for insufficient balance.
func TestSubmit_BuyEntryFee_BaseAssetCommissionValuedAndNetted(t *testing.T) {
	provider := &fakeProvider{
		placeResp: execution.OrderResponse{
			ClientOrderID:   "cry5-1",
			ExchangeOrderID: 555,
			Symbol:          "BTCUSDT",
			Side:            execution.SideBuy,
			Status:          execution.StatusFilled,
			FilledQty:       decimal.RequireFromString("0.001"),
			AvgFillPrice:    decimal.NewFromInt(70000),
			Fills: []execution.Fill{
				{
					Price:           decimal.NewFromInt(70000),
					Qty:             decimal.RequireFromString("0.001"),
					Commission:      decimal.RequireFromString("0.000001"),
					CommissionAsset: "BTC",
				},
			},
		},
	}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "BUY", 100)

	tradeID, err := exec.submitOpen(context.Background(), ps)
	if err != nil {
		t.Fatalf("submitOpen: %v", err)
	}

	var trade models.Trade
	db.First(&trade, tradeID)
	// EXP-1: 0.000001 BTC * 70000 = 0.07 USDT (no longer recorded as 0).
	if want := decimal.RequireFromString("0.07"); !trade.EntryFee.Equal(want) {
		t.Fatalf("entry_fee=%s want %s (base commission valued in USDT)", trade.EntryFee, want)
	}
	// EXP-2: quantity = 0.001 - 0.000001 = 0.000999 (net of base commission).
	if want := decimal.RequireFromString("0.000999"); !trade.Quantity.Equal(want) {
		t.Fatalf("trade qty=%s want %s (net of base commission)", trade.Quantity, want)
	}

	var pos models.Position
	if err := db.Where("symbol = ?", "BTCUSDT").First(&pos).Error; err != nil {
		t.Fatalf("load position: %v", err)
	}
	if want := decimal.RequireFromString("0.000999"); !pos.Quantity.Equal(want) {
		t.Fatalf("position qty=%s want %s", pos.Quantity, want)
	}
}

// CRY-35: a third-asset (BNB) commission is valued through the pricer.
func TestFeeFromFillsValued_ThirdAssetPricer(t *testing.T) {
	dec := decimal.RequireFromString
	fills := []execution.Fill{
		{Commission: dec("0.0001"), CommissionAsset: "BNB", Price: dec("70000")},
		{Commission: dec("0.0002"), CommissionAsset: "BNB", Price: dec("70000")},
	}
	priced := func(asset string) (decimal.Decimal, bool) {
		if asset == "BNB" {
			return dec("600"), true
		}
		return decimal.Zero, false
	}
	// (0.0001 + 0.0002) BNB × 600 = 0.18 USDT
	if fee, unv := feeFromFillsValued(fills, "USDT", "BTC", priced); unv || !fee.Equal(dec("0.18")) {
		t.Fatalf("fee=%s unvalued=%v want 0.18/false", fee, unv)
	}
	noPrice := func(string) (decimal.Decimal, bool) { return decimal.Zero, false }
	if fee, unv := feeFromFillsValued(fills, "USDT", "BTC", noPrice); !unv || !fee.IsZero() {
		t.Fatalf("fee=%s unvalued=%v want 0/true when the pricer fails", fee, unv)
	}
}

func TestTickerPricer_MemoizesAndFailsSoft(t *testing.T) {
	dec := decimal.RequireFromString
	p := &fakeProvider{tickerPrice: dec("600")}
	price := tickerPricer(context.Background(), p)
	for i := 0; i < 3; i++ {
		if got, ok := price("BNB"); !ok || !got.Equal(dec("600")) {
			t.Fatalf("price=%s ok=%v want 600/true", got, ok)
		}
	}
	if len(p.tickerCalls) != 1 || p.tickerCalls[0] != "BNBUSDT" {
		t.Fatalf("ticker calls=%v want one BNBUSDT lookup", p.tickerCalls)
	}

	failing := &fakeProvider{tickerErr: errors.New("boom")}
	price = tickerPricer(context.Background(), failing)
	price("BNB")
	if _, ok := price("BNB"); ok || len(failing.tickerCalls) != 1 {
		t.Fatalf("failed lookup must report !ok and not retry within one order (calls=%d)", len(failing.tickerCalls))
	}
	if tickerPricer(context.Background(), nil) != nil {
		t.Fatal("nil provider must yield a nil pricer")
	}
}

// End to end: BNB fee on → EntryFee valued via the ticker, and the held
// quantity is the full fill (no base-asset commission to net out).
func TestSubmitOpen_BNBCommissionValuedViaTicker(t *testing.T) {
	provider := &fakeProvider{
		placeResp: filledBuy("0.00286", "70000",
			execution.Fill{Price: decimal.RequireFromString("70000"), Qty: decimal.RequireFromString("0.00286"),
				Commission: decimal.RequireFromString("0.00025"), CommissionAsset: "BNB"}),
		tickerPrice: decimal.RequireFromString("600"),
	}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "BUY", 50)
	tradeID, err := exec.submitOpen(context.Background(), ps)
	if err != nil {
		t.Fatalf("submitOpen: %v", err)
	}
	var trade models.Trade
	db.First(&trade, tradeID)
	// 0.00025 BNB × 600 = 0.15 USDT
	if !trade.EntryFee.Equal(decimal.RequireFromString("0.15")) {
		t.Fatalf("entry_fee=%s want 0.15", trade.EntryFee)
	}
	if !trade.Quantity.Equal(decimal.RequireFromString("0.00286")) {
		t.Fatalf("qty=%s want 0.00286 (BNB fee does not reduce held base)", trade.Quantity)
	}
}
