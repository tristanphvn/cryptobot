package handlers

import (
	"net/http"
	"sync"
	"time"

	"cryptobot/internal/backtest"

	"github.com/gin-gonic/gin"
)

type BacktestHandler struct {
	engine *backtest.Engine

	mu      sync.RWMutex
	running bool
	latest  *backtest.Result
}

func NewBacktestHandler(engine *backtest.Engine) *BacktestHandler {
	return &BacktestHandler{engine: engine}
}

type backtestRequest struct {
	Symbol   string  `json:"symbol"    binding:"required"`
	Interval string  `json:"interval"  binding:"required"`
	Start    string  `json:"start"`     // RFC3339 or empty for all data
	End      string  `json:"end"`       // RFC3339 or empty for now
	Capital  float64 `json:"capital"`   // default 10000
	Window   int     `json:"window"`    // default 100
	StepSize int     `json:"step_size"` // default 1
}

// RunBacktest triggers a new backtest. Only one can run at a time.
// POST /api/v1/backtest
func (h *BacktestHandler) RunBacktest(c *gin.Context) {
	var req backtestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	h.mu.RLock()
	if h.running {
		h.mu.RUnlock()
		c.JSON(http.StatusConflict, gin.H{"error": "a backtest is already running"})
		return
	}
	h.mu.RUnlock()

	cfg := backtest.Config{
		Symbol:   req.Symbol,
		Interval: req.Interval,
		Capital:  req.Capital,
		Window:   req.Window,
		StepSize: req.StepSize,
	}

	if req.Start != "" {
		t, err := time.Parse(time.RFC3339, req.Start)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid start time, use RFC3339"})
			return
		}
		cfg.StartTime = t
	}
	if req.End != "" {
		t, err := time.Parse(time.RFC3339, req.End)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid end time, use RFC3339"})
			return
		}
		cfg.EndTime = t
	}

	h.mu.Lock()
	h.running = true
	h.mu.Unlock()

	// Run async so we don't block the HTTP handler
	go func() {
		result, err := h.engine.Run(c.Request.Context(), cfg)

		h.mu.Lock()
		defer h.mu.Unlock()
		h.running = false

		if err != nil {
			h.latest = &backtest.Result{
				Config:      cfg,
				Error:       err.Error(),
				StartedAt:   time.Now(),
				CompletedAt: time.Now(),
			}
			return
		}
		h.latest = result
	}()

	c.JSON(http.StatusAccepted, gin.H{
		"status":  "started",
		"message": "backtest running in background — poll GET /api/v1/backtest for results",
	})
}

// GetBacktestResult returns the latest backtest result.
// GET /api/v1/backtest
func (h *BacktestHandler) GetBacktestResult(c *gin.Context) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.running {
		c.JSON(http.StatusOK, gin.H{"status": "running"})
		return
	}

	if h.latest == nil {
		c.JSON(http.StatusOK, gin.H{"status": "no backtest has been run yet"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status": "completed",
		"result": h.latest,
	})
}
