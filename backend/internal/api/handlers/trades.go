package handlers

import (
	"net/http"

	"cryptobot/internal/database/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type TradeHandler struct {
	db     *gorm.DB
	prices PriceSource
	exits  ExitStatusSource
}

// NewTradeHandler wires the handler. prices and exits may be nil.
func NewTradeHandler(db *gorm.DB, prices PriceSource, exits ExitStatusSource) *TradeHandler {
	return &TradeHandler{db: db, prices: prices, exits: exits}
}

// ListTrades returns paginated trade history.
func (h *TradeHandler) ListTrades(c *gin.Context) {
	var trades []models.Trade
	h.db.Order("created_at DESC").Limit(50).Find(&trades)
	c.JSON(http.StatusOK, trades)
}

// ListPositions returns all open positions.
func (h *TradeHandler) ListPositions(c *gin.Context) {
	c.JSON(http.StatusOK, openPositionViews(c.Request.Context(), h.db, h.prices, h.exits))
}
