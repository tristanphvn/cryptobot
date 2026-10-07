package executor

import (
	"context"
	"testing"

	"cryptobot/internal/ai"
	"cryptobot/internal/database/models"
	"cryptobot/internal/riskmanager"
	"cryptobot/internal/strategy"

	"github.com/shopspring/decimal"
)

func paperBuyEval(price float64) *strategy.EvalResult {
	return &strategy.EvalResult{
		AnalysisResult: &ai.AnalysisResult{
			Signal: &ai.Signal{Action: "BUY", Confidence: 0.9, Reasoning: "test", TakeProfit: price * 1.05},
		},
		Market: ai.MarketContext{Symbol: "BTCUSDT", CurrentPrice: price},
	}
}

// CRY-32: paper fees default to 0.1% per leg — the rate live pays with
// BNB-fee off — not the 0.075% BNB-discount rate.
func TestPaperFee_DefaultIsLiveRate(t *testing.T) {
	if !DefaultPaperFeeRate.Equal(decimal.RequireFromString("0.001")) {
		t.Fatalf("DefaultPaperFeeRate=%s want 0.001", DefaultPaperFeeRate)
	}
	db := newIsolatedTestDB(t)
	risk := riskmanager.New(db, riskmanager.Config{MaxPositionPct: 2, DailyDrawdownPct: 5, MaxOpenPositions: 3})
	exec := NewExecutor(db, nil, risk, ModePaper, nil)

	id, err := exec.openTrade(context.Background(), "BTCUSDT", paperBuyEval(50000))
	if err != nil {
		t.Fatalf("openTrade: %v", err)
	}
	var trade models.Trade
	db.First(&trade, *id)
	// 2% of 10 000 = 200 USDT notional → 0.20 USDT fee.
	if !trade.EntryFee.Equal(decimal.RequireFromString("0.2")) {
		t.Fatalf("entry_fee=%s want 0.2 (200 × 0.1%%)", trade.EntryFee)
	}
}

func TestPaperFee_Override(t *testing.T) {
	db := newIsolatedTestDB(t)
	risk := riskmanager.New(db, riskmanager.Config{MaxPositionPct: 2, DailyDrawdownPct: 5, MaxOpenPositions: 3})
	exec := NewExecutor(db, nil, risk, ModePaper, nil).
		WithPaperFeeRate(decimal.RequireFromString("0.00075"))

	id, err := exec.openTrade(context.Background(), "BTCUSDT", paperBuyEval(50000))
	if err != nil {
		t.Fatalf("openTrade: %v", err)
	}
	var trade models.Trade
	db.First(&trade, *id)
	if !trade.EntryFee.Equal(decimal.RequireFromString("0.15")) {
		t.Fatalf("entry_fee=%s want 0.15 (200 × 0.075%%)", trade.EntryFee)
	}

	// Non-positive override is ignored.
	exec.WithPaperFeeRate(decimal.Zero)
	if !exec.feeRate.Equal(decimal.RequireFromString("0.00075")) {
		t.Fatalf("feeRate=%s, zero override must be ignored", exec.feeRate)
	}
}
