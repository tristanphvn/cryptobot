package api

import (
	"net/http"
	"time"

	"cryptobot/internal/ai"
	"cryptobot/internal/api/handlers"
	"cryptobot/internal/api/middleware"
	"cryptobot/internal/approval"
	"cryptobot/internal/backtest"
	"cryptobot/internal/marketprice"
	"cryptobot/internal/riskmanager"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AttachHealth wires a /health endpoint that reports liveness for each
// named subsystem. The returned map per probe is rendered as JSON so the
// frontend (or k8s) can detect a stalled realtime pipeline at a glance.
func AttachHealth(r *gin.Engine, probes map[string]func() map[string]any) {
	startedAt := time.Now()
	r.GET("/health", func(c *gin.Context) {
		out := gin.H{
			"status":    "ok",
			"uptime_s":  time.Since(startedAt).Seconds(),
			"timestamp": time.Now().UTC().Format(time.RFC3339),
		}
		for name, probe := range probes {
			out[name] = probe()
		}
		c.JSON(http.StatusOK, out)
	})
}

// NewRouter wires every public route. The approval endpoints
// (/signals/pending, /signals/:id/approve, /signals/:id/reject) sit
// behind middleware.Bearer when approvalToken is non-empty — when it
// is empty (dry-run / paper modes don't require it) the routes are
// still mounted but unauthenticated. In live mode, config.Load() has
// already enforced a non-empty token at startup.
func NewRouter(
	db *gorm.DB,
	hub *handlers.Hub,
	risk *riskmanager.RiskManager,
	btEngine *backtest.Engine,
	sm *approval.StateMachine,
	submitter handlers.Submitter,
	aiHealth *ai.HealthMonitor,
	prices *marketprice.Store,
	exits handlers.ExitStatusSource,
	approvalToken string,
	trustedProxies []string,
	allowedOrigins []string,
) *gin.Engine {
	r := gin.Default()
	// Lock down the proxy trust chain. Default gin trusts ALL proxies,
	// which means a request's X-Forwarded-For header is treated as
	// authoritative for ClientIP() — spoofable when the backend is
	// directly bound. Standard, not a bandaid: trust nobody unless the
	// operator explicitly names a proxy CIDR in TRUSTED_PROXIES.
	if err := r.SetTrustedProxies(trustedProxies); err != nil {
		// SetTrustedProxies only errors on malformed CIDR strings — fail
		// fast at startup so the operator notices the typo.
		panic("router: invalid TRUSTED_PROXIES: " + err.Error())
	}
	r.Use(middleware.CORS(allowedOrigins))

	// WebSocket
	r.GET("/ws", hub.HandleWS)

	dash := handlers.NewDashboardHandler(db)
	trades := handlers.NewTradeHandler(db, prices, exits)
	analytics := handlers.NewAnalyticsHandler(db)
	audit := handlers.NewAuditHandler(db)
	riskH := handlers.NewRiskHandler(risk)
	bt := handlers.NewBacktestHandler(btEngine)
	approvalH := handlers.NewApprovalHandler(sm, submitter, hub)
	portfolio := handlers.NewPortfolioHandler(db, prices, exits)

	v1 := r.Group("/api/v1")
	{
		v1.GET("/stats", dash.GetStats)
		v1.GET("/candles", dash.GetCandles)
		v1.GET("/trades", trades.ListTrades)
		v1.GET("/trades/:id/audit", audit.GetTradeAuditLog)
		v1.GET("/positions", trades.ListPositions)
		v1.GET("/analytics/history", analytics.GetHistory)
		// CRY-11 — portfolio: per-symbol breakdown + aggregate totals +
		// daily cumulative NetPnL series.
		v1.GET("/portfolio", portfolio.GetPortfolio)

		// Audit trail
		v1.GET("/audit-logs", audit.ListAuditLogs)
		v1.GET("/audit-logs/:id", audit.GetAuditLog)

		// Risk management
		v1.GET("/risk/status", riskH.GetRiskStatus)

		// Backtesting
		v1.GET("/backtest", bt.GetBacktestResult)

		// CRY-7: AI provider liveness — public (no bearer) so monitoring
		// tools can scrape without secrets. Cached snapshot, no
		// downstream call per request.
		if aiHealth != nil {
			aiH := handlers.NewAIHealthHandler(aiHealth)
			v1.GET("/ai/health", aiH.Get)
		}
	}

	// CRY-75: state-changing operator actions — re-arming the circuit
	// breaker and launching a (slow, Ollama-bound) backtest — require the
	// same bearer token as the approval gate whenever one is configured
	// (always in live mode).
	ops := r.Group("/api/v1")
	if approvalToken != "" {
		ops.Use(middleware.Bearer(approvalToken))
	}
	{
		ops.POST("/risk/reset", riskH.ResetCircuitBreaker)
		ops.POST("/backtest", bt.RunBacktest)
	}

	// Approval gate (CRY-5). Mount with bearer auth when configured.
	signals := r.Group("/api/v1/signals")
	if approvalToken != "" {
		signals.Use(middleware.Bearer(approvalToken))
	}
	{
		signals.GET("/pending", approvalH.ListPending)
		signals.POST("/:id/approve", approvalH.Approve)
		signals.POST("/:id/reject", approvalH.Reject)
	}

	return r
}
