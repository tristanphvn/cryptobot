package executor

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"

	"cryptobot/internal/ai"
	"cryptobot/internal/database/models"
	"cryptobot/internal/riskmanager"
	"cryptobot/internal/strategy"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// newIsolatedTestDB is a per-call variant of newTestDB: each invocation
// gets a unique DSN so `go test -count=N` doesn't share state across
// iterations (the package's stock helper keys cache by t.Name() which
// stays constant under -count, producing accumulating rows).
var isolatedTestDBSeq int64

func newIsolatedTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	n := atomic.AddInt64(&isolatedTestDBSeq, 1)
	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", t.Name(), n)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(
		&models.PendingSignal{},
		&models.Trade{},
		&models.Position{},
		&models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// TestOpenTrade_PaperUsesMarketPrice locks down the regression that
// produced the bad ETH entry on 2026-06-10 10:03: paper BUY recorded
// EntryPrice = signal.StopLoss (a placeholder) instead of the analyzer's
// market price. Verifies the trade row now reflects the price the AI
// actually saw at signal time.
func TestOpenTrade_PaperUsesMarketPrice(t *testing.T) {
	db := newIsolatedTestDB(t)
	risk := riskmanager.New(db, riskmanager.Config{
		MaxPositionPct: 2.0, DailyDrawdownPct: 5.0, MaxOpenPositions: 3,
	})
	exec := NewExecutor(db, nil, risk, ModePaper, nil)

	eval := &strategy.EvalResult{
		AnalysisResult: &ai.AnalysisResult{
			Signal: &ai.Signal{
				Action:     "BUY",
				Confidence: 0.9,
				Reasoning:  "test",
				StopLoss:   1561.22, // would have been miswritten as EntryPrice pre-fix
				TakeProfit: 1700.00,
			},
		},
		Market: ai.MarketContext{
			Symbol:       "ETHUSDT",
			CurrentPrice: 1637.75,
		},
	}

	tradeIDPtr, err := exec.openTrade(context.Background(), "ETHUSDT", eval)
	if err != nil {
		t.Fatalf("openTrade: %v", err)
	}
	if tradeIDPtr == nil {
		t.Fatal("expected trade id, got nil")
	}

	var trade models.Trade
	if err := db.First(&trade, *tradeIDPtr).Error; err != nil {
		t.Fatalf("load trade: %v", err)
	}
	wantEntry := decimal.NewFromFloat(1637.75)
	if !trade.EntryPrice.Equal(wantEntry) {
		t.Fatalf("entry_price=%s want %s (market price, NOT signal.StopLoss=1561.22)",
			trade.EntryPrice, wantEntry)
	}
	if trade.Status != models.TradeStatusOpen {
		t.Fatalf("trade.status=%s want OPEN", trade.Status)
	}
	if !trade.IsPaper {
		t.Fatal("trade.IsPaper=false want true for paper mode")
	}

	// EntryCost = market price * qty. Confirms sizing also used market
	// price, not the SL placeholder.
	if trade.EntryCost.IsZero() {
		t.Fatal("entry_cost is zero")
	}
	expectedCost := wantEntry.Mul(trade.Quantity)
	if !trade.EntryCost.Equal(expectedCost) {
		t.Fatalf("entry_cost=%s want %s (market price × qty)", trade.EntryCost, expectedCost)
	}

	// Position row mirrors the trade.
	var pos models.Position
	if err := db.Where("symbol = ? AND is_open = ?", "ETHUSDT", true).First(&pos).Error; err != nil {
		t.Fatalf("load position: %v", err)
	}
	if !pos.AvgEntryPrice.Equal(wantEntry) {
		t.Fatalf("position.avg_entry_price=%s want %s", pos.AvgEntryPrice, wantEntry)
	}
	if pos.StopLoss == nil || !pos.StopLoss.Equal(decimal.NewFromFloat(1561.22)) {
		t.Fatalf("position.stop_loss=%v want 1561.22 (signal SL still snapshotted)", pos.StopLoss)
	}
}

// TestOpenTrade_PaperRejectsZeroMarketPrice ensures the defensive guard
// fires when the analyzer fails to populate CurrentPrice — better to fail
// loud than book a Trade at $0 (or worse, silently fall back to SL).
func TestOpenTrade_PaperRejectsZeroMarketPrice(t *testing.T) {
	db := newIsolatedTestDB(t)
	risk := riskmanager.New(db, riskmanager.Config{
		MaxPositionPct: 2.0, DailyDrawdownPct: 5.0, MaxOpenPositions: 3,
	})
	exec := NewExecutor(db, nil, risk, ModePaper, nil)

	eval := &strategy.EvalResult{
		AnalysisResult: &ai.AnalysisResult{
			Signal: &ai.Signal{
				Action: "BUY", Confidence: 0.9, StopLoss: 1500, TakeProfit: 1700,
			},
		},
		Market: ai.MarketContext{Symbol: "ETHUSDT", CurrentPrice: 0},
	}

	if _, err := exec.openTrade(context.Background(), "ETHUSDT", eval); err == nil {
		t.Fatal("expected error on zero market price, got nil")
	}

	var count int64
	db.Model(&models.Trade{}).Count(&count)
	if count != 0 {
		t.Fatalf("expected 0 trade rows, got %d (defensive path leaked a write)", count)
	}
}

// TestOpenTrade_PaperEntryPriceProperty is a property-style sweep across
// a wide spread of (market, stopLoss, takeProfit) tuples. The invariant:
// EntryPrice == market, never == stopLoss, even when stopLoss is close
// enough to the market to be plausibly confused with it. 256 random
// triples covers the full Binance Spot price range (sub-cent meme coins
// through six-figure BTC) so future refactors of the sizing / fee path
// cannot quietly reintroduce the SL placeholder.
func TestOpenTrade_PaperEntryPriceProperty(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) // deterministic seed; flake-free
	for i := range 256 {
		// Market price spans 1e-4 .. 1e5 USDT.
		market := math.Pow(10, -4+rng.Float64()*9)
		// SL within ±10% of market — the realistic range where the
		// original bug was hardest to spot by eye.
		sl := market * (1 - (rng.Float64()*0.10 + 0.001))
		tp := market * (1 + (rng.Float64()*0.10 + 0.001))

		db := newIsolatedTestDB(t)
		risk := riskmanager.New(db, riskmanager.Config{
			MaxPositionPct: 2.0, DailyDrawdownPct: 5.0, MaxOpenPositions: 3,
		})
		exec := NewExecutor(db, nil, risk, ModePaper, nil)

		eval := &strategy.EvalResult{
			AnalysisResult: &ai.AnalysisResult{
				Signal: &ai.Signal{
					Action: "BUY", Confidence: 0.9,
					StopLoss: sl, TakeProfit: tp,
				},
			},
			Market: ai.MarketContext{Symbol: "TESTUSDT", CurrentPrice: market},
		}
		tradeIDPtr, err := exec.openTrade(context.Background(), "TESTUSDT", eval)
		if err != nil {
			t.Fatalf("iter %d (market=%g sl=%g): openTrade: %v", i, market, sl, err)
		}
		var trade models.Trade
		if err := db.First(&trade, *tradeIDPtr).Error; err != nil {
			t.Fatalf("iter %d: load trade: %v", i, err)
		}
		wantMarket := decimal.NewFromFloat(market)
		gotSL := decimal.NewFromFloat(sl)
		// Tolerant compare on market — float→decimal→SQLite roundtrip
		// introduces ULP-level drift that's irrelevant to the regression
		// we're guarding. 1e-9 relative is six orders of magnitude tighter
		// than any conceivable confusion with sl (which sits ≥10 bps away).
		drift := trade.EntryPrice.Sub(wantMarket).Abs().Div(wantMarket).InexactFloat64()
		if drift > 1e-9 {
			t.Fatalf("iter %d: entry_price=%s drifted %g from market=%s (sl=%s)",
				i, trade.EntryPrice, drift, wantMarket, gotSL)
		}
		// Strict: stored price must NOT equal SL — that was the bug.
		if trade.EntryPrice.Equal(gotSL) {
			t.Fatalf("iter %d: entry_price collided with SL=%s — regression to placeholder behaviour", i, gotSL)
		}
		// Strict: stored price must be much closer to market than to SL.
		distToMarket := trade.EntryPrice.Sub(wantMarket).Abs()
		distToSL := trade.EntryPrice.Sub(gotSL).Abs()
		if distToMarket.GreaterThanOrEqual(distToSL) {
			t.Fatalf("iter %d: entry_price=%s closer to sl=%s than to market=%s",
				i, trade.EntryPrice, gotSL, wantMarket)
		}
	}
}

// TestOpenTrade_PaperConcurrentDifferentSymbols stresses parallel BUYs
// on distinct symbols. Each goroutine should produce exactly one Trade
// + Position pair with EntryPrice matching its own market price — no
// cross-contamination of prices between rows.
func TestOpenTrade_PaperConcurrentDifferentSymbols(t *testing.T) {
	db := newIsolatedTestDB(t)
	risk := riskmanager.New(db, riskmanager.Config{
		MaxPositionPct: 2.0, DailyDrawdownPct: 5.0, MaxOpenPositions: 50,
	})
	exec := NewExecutor(db, nil, risk, ModePaper, nil)

	const N = 24
	type want struct {
		symbol string
		price  float64
	}
	wants := make([]want, N)
	for i := range N {
		wants[i] = want{
			symbol: fmt.Sprintf("SYM%dUSDT", i),
			price:  float64(100 + i*17), // integer-clean, distinct, no float drift
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, N)
	wg.Add(N)
	for _, w := range wants {
		go func() {
			defer wg.Done()
			eval := &strategy.EvalResult{
				AnalysisResult: &ai.AnalysisResult{
					Signal: &ai.Signal{
						Action: "BUY", Confidence: 0.9,
						StopLoss: w.price * 0.95, TakeProfit: w.price * 1.05,
					},
				},
				Market: ai.MarketContext{Symbol: w.symbol, CurrentPrice: w.price},
			}
			if _, err := exec.openTrade(context.Background(), w.symbol, eval); err != nil {
				errs <- fmt.Errorf("%s: %w", w.symbol, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent openTrade error: %v", e)
	}

	// Each symbol must end with exactly one Trade row at its own price.
	for _, w := range wants {
		var trade models.Trade
		if err := db.Where("symbol = ?", w.symbol).First(&trade).Error; err != nil {
			t.Fatalf("%s: missing trade row: %v", w.symbol, err)
		}
		wantPrice := decimal.NewFromFloat(w.price)
		if !trade.EntryPrice.Equal(wantPrice) {
			t.Fatalf("%s: entry_price=%s want %s (cross-contamination?)",
				w.symbol, trade.EntryPrice, wantPrice)
		}
		var n int64
		db.Model(&models.Trade{}).Where("symbol = ?", w.symbol).Count(&n)
		if n != 1 {
			t.Fatalf("%s: got %d trades, want 1", w.symbol, n)
		}
	}
}
