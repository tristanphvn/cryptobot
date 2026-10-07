package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cryptobot/internal/database/models"
	"cryptobot/internal/executor"
	"cryptobot/internal/marketprice"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func newPositionsDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&models.Position{}, &models.Trade{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// seedOpen writes an OPEN DOGE position + trade: 1000 @ 0.12, fee 0.12.
func seedOpen(t *testing.T, db *gorm.DB) {
	t.Helper()
	db.Create(&models.Trade{
		Symbol: "DOGEUSDT", Side: models.TradeSideBuy, Status: models.TradeStatusOpen,
		EntryPrice: dec("0.12"), Quantity: dec("1000"), EntryCost: dec("120"), EntryFee: dec("0.12"),
		EntryAt: time.Now(),
	})
	db.Create(&models.Position{
		Symbol: "DOGEUSDT", Side: models.TradeSideBuy, Quantity: dec("1000"),
		AvgEntryPrice: dec("0.12"), IsOpen: true,
	})
}

func TestOpenPositionViews_MarkToMarketNetOfEntryFee(t *testing.T) {
	db := newPositionsDB(t)
	seedOpen(t, db)
	prices := marketprice.New()
	prices.Set("DOGEUSDT", dec("0.128765"), time.Now())

	views := openPositionViews(context.Background(), db, prices, nil)
	if len(views) != 1 {
		t.Fatalf("views=%d want 1", len(views))
	}
	v := views[0]
	if !v.CurrentPrice.Equal(dec("0.128765")) {
		t.Fatalf("current_price=%s want 0.128765", v.CurrentPrice)
	}
	// (0.128765 − 0.12) × 1000 − 0.12 = 8.645
	if !v.UnrealizedPnL.Equal(dec("8.645")) {
		t.Fatalf("unrealized=%s want 8.645", v.UnrealizedPnL)
	}
	if !v.OpenEntryFee.Equal(dec("0.12")) {
		t.Fatalf("open_entry_fee=%s want 0.12", v.OpenEntryFee)
	}
}

func TestOpenPositionViews_StaleOrMissingQuoteLeavesZero(t *testing.T) {
	db := newPositionsDB(t)
	seedOpen(t, db)
	prices := marketprice.New()
	prices.Set("DOGEUSDT", dec("0.13"), time.Now().Add(-10*time.Minute))

	for name, src := range map[string]PriceSource{"stale": prices, "nil": nil} {
		v := openPositionViews(context.Background(), db, src, nil)[0]
		if !v.CurrentPrice.IsZero() || !v.UnrealizedPnL.IsZero() {
			t.Fatalf("%s: current=%s unrealized=%s want 0/0", name, v.CurrentPrice, v.UnrealizedPnL)
		}
	}
}

func TestGetPortfolio_ReportsUnrealized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newPositionsDB(t)
	seedOpen(t, db)
	prices := marketprice.New()
	prices.Set("DOGEUSDT", dec("0.11"), time.Now())

	r := gin.New()
	r.GET("/portfolio", NewPortfolioHandler(db, prices, nil).GetPortfolio)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/portfolio", nil))

	var resp PortfolioResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.BySymbol) != 1 {
		t.Fatalf("by_symbol=%d want 1", len(resp.BySymbol))
	}
	row := resp.BySymbol[0]
	// (0.11 − 0.12) × 1000 − 0.12 = −10.12
	if !row.UnrealizedPnL.Equal(dec("-10.12")) || !row.CurrentPrice.Equal(dec("0.11")) {
		t.Fatalf("unrealized=%s current=%s want -10.12 / 0.11", row.UnrealizedPnL, row.CurrentPrice)
	}
}

func TestBuildSummaries_KeepsPricePrecision(t *testing.T) {
	c := []models.Candle{
		{Symbol: "DOGEUSDT", OpenTime: time.Now().Add(-time.Hour), Close: dec("0.123456"), High: dec("0.1299"), Low: dec("0.1201")},
		{Symbol: "DOGEUSDT", OpenTime: time.Now(), Close: dec("0.128765"), High: dec("0.1299"), Low: dec("0.1201")},
	}
	s := buildSummaries(c, []string{"DOGEUSDT"})[0]
	if s.CurrentPrice != "0.128765" || s.StartPrice != "0.123456" || s.PnLAbsolute != "0.005309" ||
		s.High != "0.1299" || s.Low != "0.1201" {
		t.Fatalf("summary=%+v — prices must keep full precision", s)
	}
	if s.PnLPercent != "4.30" {
		t.Fatalf("pnl_percent=%s want 4.30", s.PnLPercent)
	}
}

// fakeExits reports an exit failure for one position id.
type fakeExits struct {
	posID uint
	f     executor.ExitFailure
}

func (x fakeExits) ExitFailure(_ string, posID uint) (executor.ExitFailure, bool) {
	return x.f, posID == x.posID
}

func TestOpenPositionViews_ReportsExitFailure(t *testing.T) {
	db := newPositionsDB(t)
	seedOpen(t, db)
	var pos models.Position
	db.First(&pos)

	fail := executor.ExitFailure{ExitReason: models.ExitReasonStopLoss, Reason: "exchange_error", Attempt: 3}
	v := openPositionViews(context.Background(), db, nil, fakeExits{posID: pos.ID, f: fail})[0]
	if v.ExitFailure == nil || v.ExitFailure.Attempt != 3 || v.ExitFailure.ExitReason != models.ExitReasonStopLoss {
		t.Fatalf("exit_failure=%+v want attempt 3 STOP_LOSS", v.ExitFailure)
	}

	// No failure for this position → field omitted from the JSON.
	v = openPositionViews(context.Background(), db, nil, fakeExits{posID: pos.ID + 1})[0]
	if v.ExitFailure != nil {
		t.Fatalf("exit_failure=%+v want nil", v.ExitFailure)
	}
	b, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(b, &m)
	if _, ok := m["exit_failure"]; ok {
		t.Fatal("exit_failure must be omitted when there is no failure")
	}
}
