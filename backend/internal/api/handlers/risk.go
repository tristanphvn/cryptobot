package handlers

import (
	"net/http"

	"cryptobot/internal/riskmanager"

	"github.com/gin-gonic/gin"
)

type RiskHandler struct {
	rm *riskmanager.RiskManager
}

func NewRiskHandler(rm *riskmanager.RiskManager) *RiskHandler {
	return &RiskHandler{rm: rm}
}

// GetRiskStatus returns the current risk state: halted, daily PnL, open positions, limits.
// GET /api/v1/risk/status
func (h *RiskHandler) GetRiskStatus(c *gin.Context) {
	c.JSON(http.StatusOK, h.rm.GetStatus())
}

// ResetCircuitBreaker clears the halted flag.
// POST /api/v1/risk/reset
func (h *RiskHandler) ResetCircuitBreaker(c *gin.Context) {
	h.rm.Reset()
	c.JSON(http.StatusOK, gin.H{"status": "circuit breaker reset"})
}
