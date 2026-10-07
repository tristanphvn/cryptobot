package handlers

import (
	"net/http"
	"strconv"

	"cryptobot/internal/database/models"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type DashboardHandler struct {
	db *gorm.DB
}

func NewDashboardHandler(db *gorm.DB) *DashboardHandler {
	return &DashboardHandler{db: db}
}

type DashboardStats struct {
	TotalTrades   int64           `json:"total_trades"`
	WinningTrades int64           `json:"winning_trades"`
	TotalPnL      decimal.Decimal `json:"total_pnl"`
	WinRate       float64         `json:"win_rate"`
	OpenPositions int64           `json:"open_positions"`

	// CRY-11 — surface fee impact on the dashboard. NetPnL is what
	// actually lands in the wallet (gross minus fees). GrossVsNetDiff
	// is the absolute fee drag so the dashboard can headline "you
	// paid $X in fees this period" without the operator computing it.
	NetPnL          decimal.Decimal `json:"net_pnl"`
	TotalFees       decimal.Decimal `json:"total_fees"`
	GrossVsNetDiff  decimal.Decimal `json:"gross_vs_net_diff"`
}

// GetStats returns aggregate stats for the dashboard. Single-pass SQL
// aggregation so the response time stays flat at ~5 ms regardless of
// trade count — relevant once paper-trade history accumulates over
// the 2-week CRY-12 validation window.
func (h *DashboardHandler) GetStats(c *gin.Context) {
	var stats DashboardStats

	h.db.Model(&models.Trade{}).
		Where("status = ?", models.TradeStatusClosed).
		Count(&stats.TotalTrades)

	h.db.Model(&models.Trade{}).
		Where("status = ? AND net_pnl > 0", models.TradeStatusClosed).
		Count(&stats.WinningTrades)

	if stats.TotalTrades > 0 {
		stats.WinRate = float64(stats.WinningTrades) / float64(stats.TotalTrades) * 100
	}

	// One row for the four aggregates so we don't hammer the same
	// table four times.
	var agg struct {
		TotalPnL  string
		NetPnL    string
		TotalFees string
	}
	h.db.Model(&models.Trade{}).
		Select(`
			COALESCE(SUM(pnl), 0)                    AS total_pn_l,
			COALESCE(SUM(net_pnl), 0)                AS net_pn_l,
			COALESCE(SUM(entry_fee + exit_fee), 0)   AS total_fees
		`).
		Where("status = ?", models.TradeStatusClosed).
		Row().Scan(&agg.TotalPnL, &agg.NetPnL, &agg.TotalFees)

	stats.TotalPnL, _ = decimal.NewFromString(agg.TotalPnL)
	stats.NetPnL, _ = decimal.NewFromString(agg.NetPnL)
	stats.TotalFees, _ = decimal.NewFromString(agg.TotalFees)
	stats.GrossVsNetDiff = stats.TotalPnL.Sub(stats.NetPnL)

	h.db.Model(&models.Position{}).
		Where("is_open = ?", true).
		Count(&stats.OpenPositions)

	c.JSON(http.StatusOK, stats)
}

// GetCandles returns recent candle data for charting.
//
// Query params:
//   - symbol   (default BTCUSDT)
//   - interval (default 1h)
//   - limit    (default 300, max 1000) — frontend chart window size.
//
// Result is ascending by open_time so the chart can render directly.
func (h *DashboardHandler) GetCandles(c *gin.Context) {
	symbol := c.DefaultQuery("symbol", "BTCUSDT")
	interval := c.DefaultQuery("interval", "1h")
	limit := 300
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}

	var candles []models.Candle
	h.db.Where("symbol = ? AND interval = ?", symbol, interval).
		Order("open_time DESC").
		Limit(limit).
		Find(&candles)

	// Reverse to chronological order for the chart
	for i, j := 0, len(candles)-1; i < j; i, j = i+1, j-1 {
		candles[i], candles[j] = candles[j], candles[i]
	}

	c.JSON(http.StatusOK, candles)
}
