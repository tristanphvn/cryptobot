package handlers

import (
	"net/http"

	"cryptobot/internal/ai"

	"github.com/gin-gonic/gin"
)

// AIHealthHandler exposes the latest ai.HealthMonitor snapshot via HTTP.
// Standard, not bandaid: the handler reads a cached snapshot — it does
// NOT trigger a fresh HealthCheck. A monitoring tool can scrape this
// endpoint at any frequency without spilling onto the LLM backend.
type AIHealthHandler struct {
	monitor *ai.HealthMonitor
}

func NewAIHealthHandler(monitor *ai.HealthMonitor) *AIHealthHandler {
	return &AIHealthHandler{monitor: monitor}
}

// Get returns the latest snapshot. Response shape:
//
//	{"provider": "ollama", "healthy": true, "last_check_at": "...", "breaker_state": "closed"}
//
// HTTP status is 200 even when Healthy=false — the response body carries
// the semantic. A 503 here would conflate "AI down" with "endpoint
// broken" and confuse monitoring tools.
func (h *AIHealthHandler) Get(c *gin.Context) {
	c.JSON(http.StatusOK, h.monitor.Snapshot())
}
