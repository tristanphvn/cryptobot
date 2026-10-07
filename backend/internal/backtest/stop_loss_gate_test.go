package backtest

import (
	"context"
	"testing"
	"time"

	"cryptobot/internal/ai"
	"cryptobot/internal/database/models"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// fixedSignalCache answers every lookup with the same BUY, so Run never
// reaches the (nil) analyzer.
type fixedSignalCache struct{ sig ai.Signal }

func (c fixedSignalCache) Get(string, time.Time, string) (*ai.AnalysisResult, bool) {
	s := c.sig
	return &ai.AnalysisResult{Signal: &s}, true
}
func (fixedSignalCache) Put(string, time.Time, string, *ai.AnalysisResult) {}

// CRY-37 parity: the backtester rejects the same wide-stop BUYs as live.
func TestRun_MaxStopLossGateBlocksWideStops(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&models.Candle{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	px := decimal.NewFromInt(100)
	for i := 0; i < 10; i++ {
		ot := start.Add(time.Duration(i) * time.Hour)
		db.Create(&models.Candle{Symbol: "BTCUSDT", Interval: "1h", OpenTime: ot, CloseTime: ot.Add(time.Hour - time.Second),
			Open: px, High: px, Low: px, Close: px, Volume: decimal.NewFromInt(1)})
	}

	run := func(capPct float64) *Result {
		t.Helper()
		// SL 9.5% below the 100 close; TP far away so nothing else blocks.
		e := NewEngine(db, nil).WithCache(fixedSignalCache{ai.Signal{Action: "BUY", Confidence: 0.9, StopLoss: 90.5, TakeProfit: 200}})
		res, err := e.Run(context.Background(), Config{
			Symbol: "BTCUSDT", Interval: "1h", Capital: 10000, Window: 3, StepSize: 1,
			Model: "test", MaxStopLossPct: capPct,
		})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return res
	}
	if res := run(0); res.TotalTrades == 0 {
		t.Fatal("gate off: expected the BUY to open a trade")
	}
	if res := run(5); res.TotalTrades != 0 {
		t.Fatalf("gate 5%%: trades=%d want 0 (stop 9.5%% wide)", res.TotalTrades)
	}
}
