package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* handlers on http.DefaultServeMux
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"cryptobot/internal/ai"
	"cryptobot/internal/api"
	"cryptobot/internal/api/handlers"
	"cryptobot/internal/approval"
	"cryptobot/internal/backtest"
	"cryptobot/internal/config"
	"cryptobot/internal/database"
	"cryptobot/internal/database/models"
	"cryptobot/internal/eventbus"
	"cryptobot/internal/exchange"
	"cryptobot/internal/executor"
	"cryptobot/internal/ingestion"
	"cryptobot/internal/marketprice"
	"cryptobot/internal/retention"
	"cryptobot/internal/riskmanager"
	"cryptobot/internal/strategy"

	"github.com/shopspring/decimal"
)

// supervise launches fn in its own goroutine and restarts it (with a
// short backoff) if it panics. Exits cleanly when ctx is cancelled.
// This is the safety net that guarantees a stream goroutine never
// silently dies — a single panic logs and respawns instead of leaving
// the pipeline in a half-dead state.
func supervise(ctx context.Context, wg *sync.WaitGroup, name string, fn func(context.Context)) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if ctx.Err() != nil {
				return
			}
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("[supervise %s] PANIC recovered: %v", name, r)
					}
				}()
				fn(ctx)
			}()
			if ctx.Err() != nil {
				return
			}
			log.Printf("[supervise %s] worker returned unexpectedly — restarting in 2s", name)
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// ── Database ─────────────────────────────────────────────────────
	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		log.Fatalf("migration: %v", err)
	}
	log.Println("database connected and migrated")

	// CRY-22 — one-shot backfill of peak_price / max_unrealized_pnl_pct
	// for OPEN positions that existed before the trailing-stop columns
	// were added. Idempotent (gated on `peak_price IS NULL`), so safe
	// to run on every boot. Must complete before the position monitor
	// goroutine starts so the first tick sees a populated peak.
	if err := database.BackfillPositionPeaks(context.Background(), db); err != nil {
		log.Fatalf("backfill peak: %v", err)
	}

	// ── Lifecycle context ────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup

	// ── Event bus ────────────────────────────────────────────────────
	bus := eventbus.New()
	defer bus.Close()

	// ── WebSocket Hub (subscribes to bus) ────────────────────────────
	hub := handlers.NewHub(cfg.AllowedOrigins)
	supervise(ctx, &wg, "hub", func(c context.Context) { hub.Run(c, bus) })

	// ── Persister (subscribes to bus) ────────────────────────────────
	persister := ingestion.NewPersister(db, bus, 50, 2*time.Second)
	supervise(ctx, &wg, "persister", persister.Run)

	// CRY-38: data lifecycle — first run 10 minutes after boot so startup
	// backfill finishes first, then every RetentionInterval.
	supervise(ctx, &wg, "retention", func(c context.Context) {
		retention.Loop(c, db, retention.FromConfig(cfg), database.HasTimescale, 10*time.Minute, cfg.RetentionInterval)
	})

	// ── Market price store (subscribes to bus) ───────────────────────
	// CRY-16: in-memory single-source-of-truth for the freshest known
	// price per symbol. Position monitor reads from here, NOT from a
	// candle DB poll, because persistence batching adds 1–2s of lag
	// that a risk engine cannot tolerate.
	prices := marketprice.New()
	supervise(ctx, &wg, "marketprice", func(c context.Context) { prices.Run(c, bus) })

	// ── Binance Collector (publishes to bus) ─────────────────────────
	collector := exchange.NewBinanceCollector(cfg.BinanceWSURL, bus)

	// Gap detector: on every successful reconnect (not first dial),
	// REST-backfill any candles that closed while the stream was down.
	// 24h cap protects REST quota; longer outages need a manual rebuild.
	gapDetector := ingestion.NewGapDetector(db, cfg.BinanceRESTURL, 24*time.Hour)
	collector.OnReconnect = gapDetector.FillIfGap

	// ── AI provider + strategy + executor + backtest ────────────────
	// CRY-7: factory stacks CircuitBreaker → Retry → Ollama. A typo in
	// AI_PROVIDER fails fast (log.Fatal below) — no silent fallback.
	aiProvider, err := ai.NewProvider(ai.Config{
		Provider:    cfg.AIProvider,
		OllamaHost:  cfg.OllamaHost,
		OllamaModel: cfg.OllamaModel,
	})
	if err != nil {
		log.Fatalf("ai provider: %v", err)
	}
	aiHealth := ai.NewHealthMonitor(aiProvider, 30*time.Second)
	supervise(ctx, &wg, "ai-health-monitor", aiHealth.Run)

	riskCfg := riskmanager.Config{
		MaxPositionPct:   cfg.RiskMaxPositionPct,
		DailyDrawdownPct: cfg.RiskDailyDrawdownPct,
		MaxOpenPositions: cfg.RiskMaxOpenPositions,
		PrioritySymbol:   cfg.RiskPrioritySymbol,
	}
	risk := riskmanager.New(db, riskCfg)
	log.Printf("risk manager: max_position=%.1f%% daily_drawdown=%.1f%% max_open=%d priority=%q",
		riskCfg.MaxPositionPct, riskCfg.DailyDrawdownPct, riskCfg.MaxOpenPositions, riskCfg.PrioritySymbol)

	analyzer := strategy.NewAnalyzer(db, aiProvider)

	// ── Approval gate (CRY-5) ────────────────────────────────────────
	// State machine + sweeper are constructed in every mode so the
	// /signals/* endpoints are always wired (defensive in case operator
	// flips EXECUTION_MODE between restarts and pending rows linger).
	// The executor only routes through the state machine when
	// mode == ModeLive.
	mode, err := executor.ParseMode(cfg.ExecutionMode)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	approvalCfg := approval.Config{
		TTL:      time.Duration(cfg.SignalApprovalTTLSec) * time.Second,
		QueueMax: cfg.SignalPendingMax,
	}
	stateMachine := approval.NewStateMachine(db, approvalCfg)
	log.Printf("approval: mode=%s ttl=%s queue_max=%d", mode, approvalCfg.TTL, approvalCfg.QueueMax)

	// 5s tick; executing rows older than ApprovalExecutingMaxAge are
	// crash-recovered to FAILED. Default 60s is generous for v1 (Submit
	// is sub-second); CRY-15's reviewer MUST bump this to exceed the
	// worst-case Binance poll duration before flipping to live.
	sweeper := approval.NewSweeper(db, 5*time.Second,
		time.Duration(cfg.ApprovalExecutingMaxAge)*time.Second)
	sweeper.OnExpired(func(ps models.PendingSignal) {
		hub.Broadcast(map[string]any{"type": "signal.expired", "data": ps})
	})
	sweeper.OnRecovered(func(ps models.PendingSignal) {
		hub.Broadcast(map[string]any{"type": "signal.crash_recovered", "data": ps})
	})

	var executorApproval *approval.StateMachine
	if mode == executor.ModeLive {
		executorApproval = stateMachine
	}
	exec := executor.NewExecutor(db, analyzer, risk, mode, executorApproval).
		WithNotifier(hub).
		WithMinReturnGate(cfg.MinExpectedReturnBps).
		WithMaxStopLossGate(cfg.MaxStopLossPct).
		WithPaperFeeRate(decimal.NewFromFloat(cfg.PaperFeeRate))

	// CRY-15: in live mode, wire the signed-REST Binance provider and
	// reconciler BEFORE supervising the sweeper. Standard, not a bandaid:
	// the sweeper runs an immediate crash-recovery sweep on entry, so
	// WithReconciler MUST be set before Run is goroutined. Paper/dry-run
	// never construct the provider — an accidental mode flip cannot
	// route signals onto the wire. Startup BLOCKS on exchangeInfo
	// prefetch so bad keys / wrong REST URL fail loudly before any
	// approval can be served.
	if mode == executor.ModeLive {
		orderClient := exchange.NewBinanceOrderClient(
			cfg.BinanceRESTURL, cfg.BinanceAPIKey, cfg.BinanceSecret, cfg.BinanceRecvWindowMs)
		startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
		if err := orderClient.Start(startCtx); err != nil {
			startCancel()
			log.Fatalf("binance order client: %v", err)
		}
		startCancel()
		log.Printf("binance order client ready: recv_window=%dms max_slippage=%dbps",
			cfg.BinanceRecvWindowMs, cfg.MaxSlippageBps)

		exec.WithProvider(orderClient, cfg.MaxSlippageBps)
		reconciler := executor.NewExchangeReconciler(db, stateMachine, orderClient, hub, cfg.MaxSlippageBps)
		sweeper.WithReconciler(reconciler)
	}

	supervise(ctx, &wg, "approval-sweeper", sweeper.Run)

	btEngine := backtest.NewEngine(db, analyzer)

	// ── Watchlist ────────────────────────────────────────────────────
	watchlist := []string{
		"BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT",
		"XRPUSDT", "ADAUSDT", "DOGEUSDT", "AVAXUSDT",
	}
	intervals := []string{"1m", "5m", "15m", "1h", "4h", "1d"}

	// ── Backfill (one-shot, in background; never blocks startup) ─────
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[backfill] panic: %v", r)
			}
		}()
		for _, sym := range watchlist {
			for _, iv := range intervals {
				if ctx.Err() != nil {
					return
				}
				log.Printf("[backfill] fetching %s %s...", sym, iv)
				n, err := exchange.Backfill(db, cfg.BinanceRESTURL, sym, iv, 500)
				if err != nil {
					log.Printf("[backfill] %s %s error: %v", sym, iv, err)
					continue
				}
				log.Printf("[backfill] %s %s inserted %d candles", sym, iv, n)
			}
		}
		log.Println("[backfill] complete")
	}()

	// ── Continuous streaming (one supervised goroutine per stream) ───
	for _, sym := range watchlist {
		for _, iv := range intervals {
			sym, iv := sym, iv
			name := "stream:" + sym + "@" + iv
			supervise(ctx, &wg, name, func(c context.Context) {
				collector.StreamKlines(c, sym, iv)
			})
		}
	}

	// ── Trade executor loop — one supervised goroutine per evaluated symbol.
	//
	// The watchlist above streams candle data for 8 symbols, but the
	// strategy loop only evaluates the configured set (default = CRY-12
	// spec 5 majors; override via EXECUTOR_SYMBOLS in .env) at 1h interval.
	// Each symbol runs in its own goroutine so an Ollama hiccup or analyzer
	// error on one symbol does not stall signals on the rest. Shared state
	// (executor.handleSignal, riskmanager.PreTradeCheck) is safe under
	// concurrent access — positions.symbol has a unique index that
	// serialises any race on duplicate opens.
	if len(cfg.ExecutorSymbols) == 0 {
		log.Fatal("config: EXECUTOR_SYMBOLS resolved to empty list — refusing to start with no work for the strategy loop")
	}
	log.Printf("executor: symbols=%v interval=1h cadence=%s",
		cfg.ExecutorSymbols, cfg.ExecutorTickInterval)

	// ── pprof debug surface (off by default) ─────────────────────────
	// Listens on a SEPARATE port so the debug endpoints stay off the
	// public API surface. Use during paper-mode validation / incident
	// response — exposes heap + goroutine + trace. No auth: gate via
	// firewall / docker port mapping in any environment that isn't a
	// developer's laptop.
	if cfg.EnablePprof {
		pprofSrv := &http.Server{
			Addr:              cfg.PprofAddr,
			ReadHeaderTimeout: 5 * time.Second,
		}
		log.Printf("pprof: listening on %s (debug surface — keep off in production)", cfg.PprofAddr)
		go func() {
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("pprof listener exited: %v", err)
			}
		}()
	}
	for _, sym := range cfg.ExecutorSymbols {
		sym := sym
		supervise(ctx, &wg, "executor:"+sym, func(c context.Context) {
			exec.Run(c, sym, "1h", cfg.ExecutorTickInterval)
		})
	}

	// ── Position monitor (CRY-16) ───────────────────────────────────
	// Deterministic SL/TP exit engine. Tick default 5s — short cadence
	// is correct here because the monitor is the structural guarantee
	// that the bot honours the risk plan it announced at OPEN,
	// regardless of AI behavior. Reads freshest in-memory price from
	// the marketprice store wired above. Single supervised goroutine
	// is sufficient: per-tick work is O(open positions) with no I/O
	// outside DB + in-memory map.
	monitor := executor.NewPositionMonitor(db, exec, prices, executor.MonitorConfig{
		Interval:           cfg.PositionMonitorInterval,
		TrailingStopPct:    cfg.TrailingStopPct,
		TrailingStopArmPct: cfg.TrailingStopArmPct,
		MinLockedProfitPct: cfg.MinLockedProfitPct,
	})
	supervise(ctx, &wg, "position-monitor", monitor.Run)

	// ── HTTP server ──────────────────────────────────────────────────
	var trustedProxies []string
	if cfg.TrustedProxies != "" {
		for _, p := range strings.Split(cfg.TrustedProxies, ",") {
			if p = strings.TrimSpace(p); p != "" {
				trustedProxies = append(trustedProxies, p)
			}
		}
	}
	router := api.NewRouter(db, hub, risk, btEngine, stateMachine, exec, aiHealth, prices, exec, cfg.ApprovalToken, trustedProxies, cfg.AllowedOrigins)

	// /health exposes pipeline liveness for k8s/monitoring.
	api.AttachHealth(router, map[string]func() map[string]any{
		"collector": collector.Health,
		"persister": persister.Health,
		"hub":       hub.Health,
	})

	srv := &http.Server{
		Addr:              ":" + cfg.BackendPort,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("server starting on %s", srv.Addr)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}()

	// ── Block until signal ───────────────────────────────────────────
	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)

	// Wait for supervised workers to drain.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		log.Println("all workers stopped cleanly")
	case <-time.After(15 * time.Second):
		log.Println("shutdown timeout — exiting")
	}
}
