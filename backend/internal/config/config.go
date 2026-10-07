package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	// Database
	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	// DBSSLMode is "disable" for local docker postgres, "require" for
	// managed Postgres (Neon, Supabase, RDS). Defaults to "disable" so
	// existing local-docker setups keep working without an env change.
	DBSSLMode string

	// Connection pool sizing. Defaults are tuned for a local DB; for a
	// remote pooled endpoint (Neon PgBouncer in transaction mode) lower
	// max-open below the plan cap and cap lifetimes so client conns
	// rotate before the pooler kills them.
	DBMaxOpenConns       int
	DBMaxIdleConns       int
	DBConnMaxLifetimeMin int // 0 = no limit
	DBConnMaxIdleTimeMin int // 0 = no limit

	// AI (CRY-7)
	AIProvider  string // "ollama" — env AI_PROVIDER; unknown value fails at startup
	OllamaHost  string
	OllamaModel string

	// Binance
	BinanceRESTURL       string
	BinanceWSURL         string
	BinanceAPIKey        string
	BinanceSecret        string
	BinanceRecvWindowMs  int64 // CRY-15: signed-request validity window; default 5000
	MaxSlippageBps       int64 // CRY-15: fail trade if drift between expected and fill > this; default 50 (= 0.5%)
	MinExpectedReturnBps int64 // CRY-9: block BUY signals whose (TakeProfit − price) / price × 10000 falls below this; default 30 (= 0.3%)
	// CRY-38 data lifecycle (days; 0 disables a rule). Run by the
	// retention job every RetentionInterval.
	Candle1mRetentionDays       int
	CandleIntradayRetentionDays int
	AuditTextRetentionDays      int
	RetentionInterval           time.Duration

	MaxStopLossPct float64 // CRY-37: block BUY signals whose stop-loss sits more than this % below the price; default 5.0, 0 disables
	PaperFeeRate   float64 // simulated per-leg taker fee for paper / dry-run + monitor gate; default 0.001 (= 0.1%, Binance Spot with BNB discount off)

	// Wallet — local only, never transmitted

	// Server
	BackendPort string

	// Risk Management
	RiskMaxPositionPct   float64 // max % of equity per trade
	RiskDailyDrawdownPct float64 // daily loss cap before circuit breaker
	RiskMaxOpenPositions int     // max simultaneous open positions
	RiskPrioritySymbol   string  // reserve the last open slot for this symbol (default ETHUSDT)

	// Execution mode & approval gate (CRY-5)
	ExecutionMode           string // "dry-run" | "paper" | "live"
	ApprovalToken           string // required when ExecutionMode=live
	SignalApprovalTTLSec    int    // seconds before a pending signal auto-expires
	SignalPendingMax        int    // hard cap on concurrent pending signals
	ApprovalExecutingMaxAge int    // seconds before a stuck `executing` row is crash-recovered to `failed`
	TrustedProxies          string // comma-separated CIDRs; empty = trust nobody (production-safe default)
	// AllowedOrigins are the dashboard origins accepted by CORS and the
	// WebSocket upgrade (CRY-75). ALLOWED_ORIGINS, comma-separated.
	AllowedOrigins []string

	// ExecutorSymbols is the set of symbols the strategy loop ticks on
	// at the configured cadence (one supervised goroutine per symbol).
	// Default is the CRY-12 spec 5 majors; override via EXECUTOR_SYMBOLS
	// for testing or to widen / narrow the scope without a recompile.
	// Each entry is uppercased and trimmed; empty entries are dropped.
	ExecutorSymbols []string

	// ExecutorTickInterval is the cadence at which each executor
	// goroutine fires an analyzer evaluation. Default 5m matches the
	// pre-CRY-12 behaviour. Operators on slower inference (CPU-only
	// Ollama) bump this so AI inference time stays well below the
	// tick period — back-pressure on the analyzer translates into a
	// growing backlog of stale signals, which contaminates paper-mode
	// validation metrics. Override via EXECUTOR_TICK_INTERVAL using
	// Go's time.ParseDuration syntax ("15m", "1h30m", etc.).
	ExecutorTickInterval time.Duration

	// CRY-16: position-monitor cadence. Default 5s — the monitor is a
	// risk engine reading freshest in-memory prices, NOT an inference
	// loop, so a short cadence is correct. NewPositionMonitor clamps
	// the value to [1s, 60s]; outside that range the config is normalised
	// to the boundary rather than rejected at startup.
	PositionMonitorInterval time.Duration

	// CRY-22 trailing-stop parameters, read every monitor tick:
	//
	//   TrailingStopPct      — close the position when current price falls
	//                          this many percent below the peak seen since
	//                          entry. Default 2.0.
	//   TrailingStopArmPct   — peak unrealized PnL % required before the
	//                          trail arms. Latches once crossed, so a
	//                          position that touched +1% then settled back
	//                          still trails. Default 1.0 — keeps fresh
	//                          positions from being trail-stopped out on
	//                          micro-noise around entry.
	//   MinLockedProfitPct   — trail will NOT fire unless the resulting
	//                          close locks in at least this much net post-
	//                          fee return. Prevents micro-profit churn
	//                          (position whips +1.5% then back to +0.3% —
	//                          armed by the peak but locking <0.5% net
	//                          is worse than holding for a real exit).
	//                          Default 0.5.
	//
	// Set all three to 0 to disable trailing stops entirely (the monitor
	// still updates peak_price / max_unrealized_pnl_pct columns for
	// downstream observability — they're cheap and useful for the audit
	// trail introduced in PR-2 of the same ticket).
	TrailingStopPct    float64
	TrailingStopArmPct float64
	MinLockedProfitPct float64

	// EnablePprof mounts net/http/pprof on PprofAddr in a separate
	// listener (off the main API port) for goroutine / heap / trace
	// inspection during paper-mode validation and incident response.
	// Defaults to OFF. Keep it off in production: pprof exposes
	// heap dumps + can hang the runtime via trace, and there is no
	// auth on the listener.
	EnablePprof bool
	PprofAddr   string // default ":6060"
}

func Load() (*Config, error) {
	// Load .env from project root (two levels up from the binary in Docker,
	// or from the CWD when running locally).
	_ = godotenv.Load()          // CWD
	_ = godotenv.Load("../.env") // when running from backend/

	cfg := &Config{
		DBHost:               getEnv("POSTGRES_HOST", "localhost"),
		DBPort:               getEnv("POSTGRES_PORT", "5432"),
		DBUser:               getEnv("POSTGRES_USER", "cryptobot"),
		DBPassword:           os.Getenv("POSTGRES_PASSWORD"),
		DBName:               getEnv("POSTGRES_DB", "cryptobot"),
		DBSSLMode:            getEnv("POSTGRES_SSLMODE", "disable"),
		DBMaxOpenConns:       getEnvInt("POSTGRES_MAX_OPEN_CONNS", 25),
		DBMaxIdleConns:       getEnvInt("POSTGRES_MAX_IDLE_CONNS", 5),
		DBConnMaxLifetimeMin: getEnvInt("POSTGRES_CONN_MAX_LIFETIME_MIN", 0),
		DBConnMaxIdleTimeMin: getEnvInt("POSTGRES_CONN_MAX_IDLE_MIN", 0),

		AIProvider:  getEnv("AI_PROVIDER", "ollama"),
		OllamaHost:  getEnv("OLLAMA_HOST", "http://localhost:11434"),
		OllamaModel: getEnv("OLLAMA_MODEL", "llama3"),

		BinanceRESTURL:       getEnv("BINANCE_REST_URL", "https://api.binance.com"),
		BinanceWSURL:         getEnv("BINANCE_WS_URL", "wss://stream.binance.com:9443/ws"),
		BinanceAPIKey:        os.Getenv("BINANCE_API_KEY"),
		BinanceSecret:        os.Getenv("BINANCE_API_SECRET"),
		BinanceRecvWindowMs:  int64(getEnvInt("BINANCE_RECV_WINDOW_MS", 5000)),
		MaxSlippageBps:       int64(getEnvInt("MAX_SLIPPAGE_BPS", 50)),
		MinExpectedReturnBps: int64(getEnvInt("MIN_EXPECTED_RETURN_BPS", 30)),
		MaxStopLossPct:       getEnvFloat("MAX_STOP_LOSS_PCT", 5.0),

		Candle1mRetentionDays:       getEnvInt("CANDLE_1M_RETENTION_DAYS", 30),
		CandleIntradayRetentionDays: getEnvInt("CANDLE_INTRADAY_RETENTION_DAYS", 90),
		AuditTextRetentionDays:      getEnvInt("AUDIT_TEXT_RETENTION_DAYS", 30),
		PaperFeeRate:                getEnvFloat("PAPER_FEE_RATE", 0.001),

		BackendPort: getEnv("BACKEND_PORT", "8081"),

		RiskMaxPositionPct:   getEnvFloat("RISK_MAX_POSITION_PCT", 2.0),
		RiskDailyDrawdownPct: getEnvFloat("RISK_DAILY_DRAWDOWN_PCT", 5.0),
		RiskMaxOpenPositions: getEnvInt("RISK_MAX_OPEN_POSITIONS", 3),
		RiskPrioritySymbol:   strings.ToUpper(strings.TrimSpace(getEnv("PRIORITY_SYMBOL", "ETHUSDT"))),

		ExecutionMode:           getEnv("EXECUTION_MODE", "dry-run"),
		ApprovalToken:           os.Getenv("APPROVAL_TOKEN"),
		SignalApprovalTTLSec:    getEnvInt("SIGNAL_APPROVAL_TTL_SEC", 60),
		SignalPendingMax:        getEnvInt("SIGNAL_PENDING_MAX", 10),
		ApprovalExecutingMaxAge: getEnvInt("APPROVAL_EXECUTING_MAX_AGE_SEC", 60),
		TrustedProxies:          os.Getenv("TRUSTED_PROXIES"),
		AllowedOrigins: parseList(getEnv("ALLOWED_ORIGINS",
			"http://localhost:3002,http://127.0.0.1:3002")),
		ExecutorSymbols: parseSymbolList(getEnv("EXECUTOR_SYMBOLS",
			"BTCUSDT,ETHUSDT,SOLUSDT,BNBUSDT,XRPUSDT,ADAUSDT,DOGEUSDT,AVAXUSDT")),
	}

	tickInterval, err := parseDuration(getEnv("EXECUTOR_TICK_INTERVAL", "5m"))
	if err != nil {
		return nil, fmt.Errorf("EXECUTOR_TICK_INTERVAL: %w", err)
	}
	if tickInterval <= 0 {
		return nil, fmt.Errorf("EXECUTOR_TICK_INTERVAL must be positive, got %s", tickInterval)
	}
	cfg.ExecutorTickInterval = tickInterval

	monitorInterval, err := parseDuration(getEnv("POSITION_MONITOR_INTERVAL", "5s"))
	if err != nil {
		return nil, fmt.Errorf("POSITION_MONITOR_INTERVAL: %w", err)
	}
	cfg.PositionMonitorInterval = monitorInterval

	retentionInterval, err := parseDuration(getEnv("RETENTION_INTERVAL", "24h"))
	if err != nil {
		return nil, fmt.Errorf("RETENTION_INTERVAL: %w", err)
	}
	if retentionInterval <= 0 {
		return nil, fmt.Errorf("RETENTION_INTERVAL must be positive, got %s", retentionInterval)
	}
	cfg.RetentionInterval = retentionInterval
	if cfg.Candle1mRetentionDays < 0 || cfg.CandleIntradayRetentionDays < 0 || cfg.AuditTextRetentionDays < 0 {
		return nil, fmt.Errorf("CANDLE_1M_RETENTION_DAYS / CANDLE_INTRADAY_RETENTION_DAYS / AUDIT_TEXT_RETENTION_DAYS must be ≥ 0 (0 disables)")
	}

	cfg.TrailingStopPct = getEnvFloat("TRAILING_STOP_PCT", 2.0)
	cfg.TrailingStopArmPct = getEnvFloat("TRAILING_STOP_ARM_PCT", 1.0)
	cfg.MinLockedProfitPct = getEnvFloat("MIN_LOCKED_PROFIT_PCT", 0.5)
	if cfg.TrailingStopPct < 0 || cfg.TrailingStopArmPct < 0 || cfg.MinLockedProfitPct < 0 {
		return nil, fmt.Errorf(
			"TRAILING_STOP_PCT / TRAILING_STOP_ARM_PCT / MIN_LOCKED_PROFIT_PCT must be ≥ 0 (set all three to 0 to disable)",
		)
	}

	cfg.EnablePprof = getEnvBool("ENABLE_PPROF", false)
	cfg.PprofAddr = getEnv("PPROF_ADDR", ":6060")

	if cfg.DBPassword == "" {
		return nil, fmt.Errorf("POSTGRES_PASSWORD is required")
	}

	// Validate execution-mode invariants. Standard, not bandaid: no
	// silent fallbacks — a misconfigured live deployment must fail at
	// startup so the operator never thinks live trading is on when it
	// isn't (or vice versa).
	switch cfg.ExecutionMode {
	case "dry-run", "paper", "live":
	default:
		return nil, fmt.Errorf("EXECUTION_MODE=%q invalid; want dry-run|paper|live", cfg.ExecutionMode)
	}
	if cfg.ExecutionMode == "live" {
		if cfg.ApprovalToken == "" {
			return nil, fmt.Errorf("EXECUTION_MODE=live requires APPROVAL_TOKEN")
		}
		if cfg.BinanceAPIKey == "" || cfg.BinanceSecret == "" {
			return nil, fmt.Errorf("EXECUTION_MODE=live requires BINANCE_API_KEY and BINANCE_API_SECRET")
		}
	}

	return cfg, nil
}

// DSN returns the PostgreSQL connection string.
//
// SSL mode is driven by POSTGRES_SSLMODE (default "disable" for local
// docker postgres; set to "require" for managed Postgres like Neon).
// TimeZone is pinned to UTC so timestamp semantics are identical across
// local-DB and remote-DB deployments.
func (c *Config) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=UTC",
		c.DBHost, c.DBPort, c.DBUser, c.DBPassword, c.DBName, c.DBSSLMode,
	)
}

// DSNRedacted returns DSN with the password replaced — safe to log at startup.
func (c *Config) DSNRedacted() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=*** dbname=%s sslmode=%s TimeZone=UTC",
		c.DBHost, c.DBPort, c.DBUser, c.DBName, c.DBSSLMode,
	)
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

// getEnvBool accepts the common truthy spellings (1, true, yes, on — case
// insensitive). Anything else, including the empty string, returns the
// fallback. Standard, not a bandaid: callers should not have to remember
// which spelling the env parser will accept.
func getEnvBool(key string, fallback bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	case "":
		return fallback
	}
	return fallback
}

// parseSymbolList splits a comma-separated symbol list, trims whitespace,
// uppercases each entry, and drops empties. Same normalisation as the
// cmd/backfill and cmd/backtest CLI flags so operator input is consistent
// across env vars and flags.
func parseSymbolList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.ToUpper(strings.TrimSpace(p)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseList splits a comma-separated value, trimming blanks; case kept.
func parseList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseDuration is a thin wrapper around time.ParseDuration so a missing
// or empty env var is treated identically to "use the default" rather
// than producing an ambiguous zero-duration. Callers gate on >0 after.
func parseDuration(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, fmt.Errorf("empty duration")
	}
	return time.ParseDuration(raw)
}
