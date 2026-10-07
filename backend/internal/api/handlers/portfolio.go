package handlers

import (
	"context"
	"net/http"
	"time"

	"cryptobot/internal/database/models"
	"cryptobot/internal/executor"
	"cryptobot/internal/marketprice"

	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// PriceSource is the realtime quote lookup (marketprice.Store).
type PriceSource interface {
	Get(symbol string) (marketprice.Quote, bool)
}

// ExitStatusSource reports a failing live exit for an open position
// (CRY-34). Implemented by the executor; nil when not wired.
type ExitStatusSource interface {
	ExitFailure(symbol string, posID uint) (executor.ExitFailure, bool)
}

// portfolioMaxQuoteAge — older quotes are not shown as "current".
const portfolioMaxQuoteAge = 2 * time.Minute

// PortfolioHandler powers /api/v1/portfolio — the CRY-11 view that
// surfaces fee impact at a glance.
type PortfolioHandler struct {
	db     *gorm.DB
	prices PriceSource
	exits  ExitStatusSource
}

// NewPortfolioHandler wires the handler. prices may be nil (no
// unrealized figures are then reported); exits may be nil (no exit
// failures are then reported).
func NewPortfolioHandler(db *gorm.DB, prices PriceSource, exits ExitStatusSource) *PortfolioHandler {
	return &PortfolioHandler{db: db, prices: prices, exits: exits}
}

// PortfolioBySymbol is one row of the per-symbol table. TotalInvested
// is the sum of entry_cost across all CLOSED trades for the symbol —
// "how much money have we put through this symbol over its lifetime"
// rather than current outstanding exposure. CurrentPrice / OpenQty
// are populated from the open Position row when one exists.
type PortfolioBySymbol struct {
	Symbol         string          `json:"symbol"`
	TotalInvested  decimal.Decimal `json:"total_invested"`
	RealizedPnL    decimal.Decimal `json:"realized_pnl"`
	UnrealizedPnL  decimal.Decimal `json:"unrealized_pnl"`
	TotalFees      decimal.Decimal `json:"total_fees"`
	OpenQty        decimal.Decimal `json:"open_qty"`
	CurrentPrice   decimal.Decimal `json:"current_price"`
	ClosedTrades   int64           `json:"closed_trades"`
}

// PortfolioAggregate is the totals row shown at the top of the page.
type PortfolioAggregate struct {
	TotalNetPnL     decimal.Decimal `json:"total_net_pnl"`
	TotalFees       decimal.Decimal `json:"total_fees"`
	TotalInvested   decimal.Decimal `json:"total_invested"`
	OpenPositions   int64           `json:"open_positions"`
}

// PortfolioDailyPoint is one point in the cumulative NetPnL chart.
// Date is UTC midnight of the exit day; the frontend renders the
// running sum (recharts handles the cumulation in data prep).
type PortfolioDailyPoint struct {
	Date   string          `json:"date"`    // YYYY-MM-DD UTC
	NetPnL decimal.Decimal `json:"net_pnl"` // sum of net_pnl for trades closed that day
}

// PortfolioResponse is the full response shape.
type PortfolioResponse struct {
	BySymbol    []PortfolioBySymbol   `json:"by_symbol"`
	Aggregate   PortfolioAggregate    `json:"aggregate"`
	DailyNetPnL []PortfolioDailyPoint `json:"daily_net_pnl"`
}

// GetPortfolio returns the per-symbol table, the aggregate totals, and
// the daily-bucketed cumulative-PnL series. Three SQL queries, all
// indexed — should stay under 200 ms with the documented 1 000-trade
// target.
func (h *PortfolioHandler) GetPortfolio(c *gin.Context) {
	ctx := c.Request.Context()

	// 1. Per-symbol aggregates from CLOSED trades.
	type bySymRow struct {
		Symbol        string
		TotalInvested string
		RealizedPnL   string
		TotalFees     string
		ClosedTrades  int64
	}
	var bySym []bySymRow
	h.db.WithContext(ctx).Model(&models.Trade{}).
		Select(`
			symbol,
			COALESCE(SUM(entry_cost), 0)             AS total_invested,
			COALESCE(SUM(net_pnl), 0)                AS realized_pn_l,
			COALESCE(SUM(entry_fee + exit_fee), 0)   AS total_fees,
			COUNT(*)                                 AS closed_trades
		`).
		Where("status = ?", models.TradeStatusClosed).
		Group("symbol").
		Order("symbol ASC").
		Find(&bySym)

	// Build a map symbol → result. Open-position data layered in next.
	byKey := make(map[string]*PortfolioBySymbol, len(bySym))
	for _, r := range bySym {
		ti, _ := decimal.NewFromString(r.TotalInvested)
		rp, _ := decimal.NewFromString(r.RealizedPnL)
		tf, _ := decimal.NewFromString(r.TotalFees)
		byKey[r.Symbol] = &PortfolioBySymbol{
			Symbol:        r.Symbol,
			TotalInvested: ti,
			RealizedPnL:   rp,
			TotalFees:     tf,
			ClosedTrades:  r.ClosedTrades,
		}
	}

	// 2. Open positions for unrealized + current price + open_qty.
	// CRY-33: positions.current_price / unrealized_pnl have no writer,
	// so both are computed here from the realtime quote. Unrealized is
	// net of the entry fee already paid on the open trade (exit fee not
	// yet incurred, so not deducted).
	positions := openPositionViews(ctx, h.db, h.prices, h.exits)
	for _, p := range positions {
		row, ok := byKey[p.Symbol]
		if !ok {
			row = &PortfolioBySymbol{Symbol: p.Symbol}
			byKey[p.Symbol] = row
		}
		row.OpenQty = p.Quantity
		row.CurrentPrice = p.CurrentPrice
		row.UnrealizedPnL = p.UnrealizedPnL
	}

	// 3. Aggregate over all the per-symbol rows we just built — single
	// pass, no extra DB hit. Slices are pre-initialised so the JSON
	// contract is `[]` (not `null`) when empty — the frontend renders
	// off `length` and `null.length` would crash. Standard, not a
	// bandaid: empty list as a value is not the same thing as null.
	resp := PortfolioResponse{
		BySymbol:    []PortfolioBySymbol{},
		DailyNetPnL: []PortfolioDailyPoint{},
	}
	for _, row := range byKey {
		resp.BySymbol = append(resp.BySymbol, *row)
		resp.Aggregate.TotalInvested = resp.Aggregate.TotalInvested.Add(row.TotalInvested)
		resp.Aggregate.TotalNetPnL = resp.Aggregate.TotalNetPnL.Add(row.RealizedPnL)
		resp.Aggregate.TotalFees = resp.Aggregate.TotalFees.Add(row.TotalFees)
	}
	resp.Aggregate.OpenPositions = int64(len(positions))

	// 4. Daily NetPnL series for the cumulative chart. Cap at 90 days
	// so a year of trades doesn't bloat the response — operator can
	// query a wider window via a future query param if needed.
	cutoff := time.Now().UTC().AddDate(0, 0, -90)
	type dayRow struct {
		Date   time.Time
		NetPnL string
	}
	var days []dayRow
	h.db.WithContext(ctx).Model(&models.Trade{}).
		Select(`
			DATE_TRUNC('day', exit_at AT TIME ZONE 'UTC') AS date,
			COALESCE(SUM(net_pnl), 0)                     AS net_pn_l
		`).
		Where("status = ? AND exit_at IS NOT NULL AND exit_at >= ?", models.TradeStatusClosed, cutoff).
		Group("date").
		Order("date ASC").
		Find(&days)
	for _, d := range days {
		np, _ := decimal.NewFromString(d.NetPnL)
		resp.DailyNetPnL = append(resp.DailyNetPnL, PortfolioDailyPoint{
			Date:   d.Date.Format("2006-01-02"),
			NetPnL: np,
		})
	}

	c.JSON(http.StatusOK, resp)
}

// PositionView is an open position marked to the realtime quote.
// OpenEntryFee is the entry fee already paid on the open trade, so a
// client recomputing PnL from a live price can stay net of it.
type PositionView struct {
	models.Position
	OpenEntryFee decimal.Decimal `json:"open_entry_fee"`
	// ExitFailure is set while a live stop / take-profit / trailing exit
	// of this position keeps failing to sell on the venue (CRY-34).
	ExitFailure *executor.ExitFailure `json:"exit_failure,omitempty"`
}

// openPositionViews loads open positions and fills CurrentPrice /
// UnrealizedPnL from the realtime quote (CRY-33: the DB columns have no
// writer). Unrealized is net of the entry fee already paid; the exit
// fee is not yet incurred and not deducted. Without a fresh quote both
// stay zero (rendered as "—").
func openPositionViews(ctx context.Context, db *gorm.DB, prices PriceSource, exits ExitStatusSource) []PositionView {
	var positions []models.Position
	db.WithContext(ctx).Where("is_open = ?", true).Find(&positions)

	type feeRow struct {
		Symbol   string
		EntryFee string
	}
	var rows []feeRow
	db.WithContext(ctx).Model(&models.Trade{}).
		Select("symbol, COALESCE(SUM(entry_fee), 0) AS entry_fee").
		Where("status = ?", models.TradeStatusOpen).
		Group("symbol").
		Find(&rows)
	fees := make(map[string]decimal.Decimal, len(rows))
	for _, r := range rows {
		fees[r.Symbol], _ = decimal.NewFromString(r.EntryFee)
	}

	views := make([]PositionView, 0, len(positions))
	for _, p := range positions {
		v := PositionView{Position: p, OpenEntryFee: fees[p.Symbol]}
		if prices != nil {
			if q, ok := prices.Get(p.Symbol); ok && !q.IsStale(portfolioMaxQuoteAge) {
				v.CurrentPrice = q.Price
				v.UnrealizedPnL = q.Price.Sub(p.AvgEntryPrice).Mul(p.Quantity).Sub(v.OpenEntryFee)
			}
		}
		if exits != nil {
			if f, ok := exits.ExitFailure(p.Symbol, p.ID); ok {
				v.ExitFailure = &f
			}
		}
		views = append(views, v)
	}
	return views
}
