package handlers

import (
	"net/http"
	"strings"
	"time"

	"cryptobot/internal/database/models"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type AnalyticsHandler struct {
	db *gorm.DB
}

func NewAnalyticsHandler(db *gorm.DB) *AnalyticsHandler {
	return &AnalyticsHandler{db: db}
}

// rangeConfig maps a range label to a time duration and the candle interval
// we should query to keep the payload small.
type rangeConfig struct {
	duration time.Duration
	interval string // candle interval to select
}

var rangeMap = map[string]rangeConfig{
	"1H":  {1 * time.Hour, "1m"},
	"4H":  {4 * time.Hour, "1m"},
	"24H": {24 * time.Hour, "1m"},
	"7D":  {7 * 24 * time.Hour, "1h"},
	"30D": {30 * 24 * time.Hour, "4h"},
	"90D": {90 * 24 * time.Hour, "1d"},
	"1Y":  {365 * 24 * time.Hour, "1d"},
	"ALL": {0, "1d"}, // 0 means no lower bound
}

// HistoryPoint is one data point returned per symbol per timestamp.
type HistoryPoint struct {
	Time   time.Time `json:"time"`
	Symbol string    `json:"symbol"`
	Close  string    `json:"close"`
	Volume string    `json:"volume"`
	High   string    `json:"high"`
	Low    string    `json:"low"`
}

// SymbolSummary holds aggregated stats per symbol for the requested range.
type SymbolSummary struct {
	Symbol       string `json:"symbol"`
	CurrentPrice string `json:"current_price"`
	StartPrice   string `json:"start_price"`
	PnLPercent   string `json:"pnl_percent"`
	PnLAbsolute  string `json:"pnl_absolute"`
	High         string `json:"high"`
	Low          string `json:"low"`
	AvgVolume    string `json:"avg_volume"`
}

type HistoryResponse struct {
	Points    []HistoryPoint `json:"points"`
	Summaries []SymbolSummary `json:"summaries"`
}

// GetHistory handles GET /api/v1/analytics/history?symbols=BTCUSDT,ETHUSDT&range=7D
func (h *AnalyticsHandler) GetHistory(c *gin.Context) {
	symbolsParam := c.DefaultQuery("symbols", "BTCUSDT")
	rangeParam := c.DefaultQuery("range", "7D")

	symbols := strings.Split(symbolsParam, ",")
	for i := range symbols {
		symbols[i] = strings.TrimSpace(strings.ToUpper(symbols[i]))
	}

	rc, ok := rangeMap[strings.ToUpper(rangeParam)]
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid range, use: 1H, 4H, 24H, 7D, 30D, 90D, 1Y, ALL"})
		return
	}

	// Build time filter
	query := h.db.Where("symbol IN ?", symbols)

	if rc.duration > 0 {
		since := time.Now().UTC().Add(-rc.duration)
		query = query.Where("open_time >= ?", since)
	}

	// For downsampling: we select candles matching the target interval.
	// If the exact interval isn't in the DB, fall back to the smallest
	// available interval and let the frontend handle display.
	query = query.Where("interval = ?", bestInterval(h.db, symbols, rc.interval))

	var candles []models.Candle
	query.Order("open_time ASC").Find(&candles)

	// Build response points
	points := make([]HistoryPoint, 0, len(candles))
	for _, c := range candles {
		points = append(points, HistoryPoint{
			Time:   c.OpenTime,
			Symbol: c.Symbol,
			Close:  c.Close.String(),
			Volume: c.Volume.String(),
			High:   c.High.String(),
			Low:    c.Low.String(),
		})
	}

	// Build per-symbol summaries
	summaries := buildSummaries(candles, symbols)

	c.JSON(http.StatusOK, HistoryResponse{
		Points:    points,
		Summaries: summaries,
	})
}

// bestInterval checks which interval is actually available in the DB for
// the requested symbols. Prefers the target but falls back gracefully.
func bestInterval(db *gorm.DB, symbols []string, target string) string {
	preferred := []string{target, "1h", "4h", "1d", "1m", "5m", "15m"}
	for _, iv := range preferred {
		var count int64
		db.Model(&models.Candle{}).
			Where("symbol IN ? AND interval = ?", symbols, iv).
			Count(&count)
		if count > 0 {
			return iv
		}
	}
	return target
}

func buildSummaries(candles []models.Candle, symbols []string) []SymbolSummary {
	type accum struct {
		first      decimal.Decimal
		last       decimal.Decimal
		high       decimal.Decimal
		low        decimal.Decimal
		volumeSum  decimal.Decimal
		count      int
		hasData    bool
	}

	data := make(map[string]*accum)
	for _, s := range symbols {
		data[s] = &accum{}
	}

	for _, c := range candles {
		a, ok := data[c.Symbol]
		if !ok {
			continue
		}
		if !a.hasData {
			a.first = c.Close
			a.high = c.High
			a.low = c.Low
			a.hasData = true
		}
		a.last = c.Close
		if c.High.GreaterThan(a.high) {
			a.high = c.High
		}
		if c.Low.LessThan(a.low) {
			a.low = c.Low
		}
		a.volumeSum = a.volumeSum.Add(c.Volume)
		a.count++
	}

	summaries := make([]SymbolSummary, 0, len(symbols))
	for _, s := range symbols {
		a := data[s]
		if !a.hasData {
			summaries = append(summaries, SymbolSummary{Symbol: s})
			continue
		}

		pnlAbs := a.last.Sub(a.first)
		pnlPct := decimal.Zero
		if !a.first.IsZero() {
			pnlPct = pnlAbs.Div(a.first).Mul(decimal.NewFromInt(100))
		}

		avgVol := decimal.Zero
		if a.count > 0 {
			avgVol = a.volumeSum.Div(decimal.NewFromInt(int64(a.count)))
		}

		// Prices keep full exchange precision — DOGE / ADA / XRP trade
		// below $1 and 2 decimals would erase most of the move. The
		// frontend picks display precision (CRY-33).
		summaries = append(summaries, SymbolSummary{
			Symbol:       s,
			CurrentPrice: a.last.String(),
			StartPrice:   a.first.String(),
			PnLPercent:   pnlPct.StringFixed(2),
			PnLAbsolute:  pnlAbs.String(),
			High:         a.high.String(),
			Low:          a.low.String(),
			AvgVolume:    avgVol.StringFixed(2),
		})
	}

	return summaries
}
