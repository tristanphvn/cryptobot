package backtest

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"cryptobot/internal/ai"
	"cryptobot/internal/database/models"
	"cryptobot/internal/riskmanager"
	"cryptobot/internal/strategy"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// Config defines the parameters for a backtest run. CRY-10 added FeeBps,
// MinReturnBps, and Model — together they let the backtester match the
// live executor's economics exactly: same fee drag, same min-return gate,
// same AI model fingerprint for caching.
type Config struct {
	Symbol         string    `json:"symbol"`
	Interval       string    `json:"interval"` // e.g. "1h"
	StartTime      time.Time `json:"start_time"`
	EndTime        time.Time `json:"end_time"`
	Capital        float64   `json:"capital"`           // starting capital in USD; default 10000
	Window         int       `json:"window"`            // lookback candles per evaluation; default 100
	StepSize       int       `json:"step_size"`         // candles to advance between evaluations; default 1
	FeeBps         float64   `json:"fee_bps"`           // per-leg taker fee in basis points; default 10 (Binance Spot, BNB discount off)
	MinReturnBps   int64     `json:"min_return_bps"`    // CRY-9 gate; 0 = disabled
	MaxStopLossPct float64   `json:"max_stop_loss_pct"` // CRY-37 gate; 0 = disabled
	Model          string    `json:"model"`             // AI model name (cache-key component)
	ConfidenceMin  float64   `json:"confidence_min"`    // override default 0.6 (live gate) if set

	// PROF-6: position size is sourced from the live risk default rather
	// than a hardcoded 2% so backtest and live stay in lockstep when
	// RISK_MAX_POSITION_PCT changes. 0 → riskmanager.DefaultConfig().
	MaxPositionPct float64 `json:"max_position_pct"`
	// PROF-4: per-leg slippage in basis points, applied adverse to the
	// fill (BUY higher, SELL lower). 0 = slippage-free (optimistic; logged).
	SlippageBps float64 `json:"slippage_bps"`
	// PROF-1: trailing-stop knobs mirrored from the live monitor so the
	// backtest can replay the same exit. All default 0 (disabled), matching
	// the live default where a zero TrailingStopPct disables firing.
	TrailingStopPct    float64 `json:"trailing_stop_pct"`
	TrailingStopArmPct float64 `json:"trailing_stop_arm_pct"`
	MinLockedProfitPct float64 `json:"min_locked_profit_pct"`
}

// TradeRecord captures a single simulated trade. CRY-10 added EntryFee /
// ExitFee / NetPnL so the report can show gross vs net side-by-side and
// the go/no-go check operates on real fee-aware figures.
type TradeRecord struct {
	EntryTime  time.Time       `json:"entry_time"`
	ExitTime   time.Time       `json:"exit_time"`
	Side       string          `json:"side"`
	EntryPrice decimal.Decimal `json:"entry_price"`
	ExitPrice  decimal.Decimal `json:"exit_price"`
	Quantity   decimal.Decimal `json:"quantity"`
	EntryFee   decimal.Decimal `json:"entry_fee"`
	ExitFee    decimal.Decimal `json:"exit_fee"`
	PnL        decimal.Decimal `json:"pnl"`         // gross (exit − entry) × qty
	PnLPct     decimal.Decimal `json:"pnl_pct"`     // gross percent
	NetPnL     decimal.Decimal `json:"net_pnl"`     // gross − fees
	NetPnLPct  decimal.Decimal `json:"net_pnl_pct"` // net % over entry notional
	Confidence float64         `json:"confidence"`
	Reasoning  string          `json:"reasoning"`
	// ExitReason records HOW the position closed: STOP_LOSS / TRAILING_STOP
	// / TAKE_PROFIT (deterministic monitor replay), AI_SELL, or END_OF_DATA
	// (PROF-1 / PROF-3). Empty on legacy records.
	ExitReason string `json:"exit_reason,omitempty"`
}

// Result is the output of a backtest run. CRY-10 added Sortino, longest
// losing streak, net figures, fee total, and the structured Verdict so the
// CLI / handler can both display a PASS/FAIL line without re-deriving it.
type Result struct {
	Config              Config        `json:"config"`
	Trades              []TradeRecord `json:"trades"`
	TotalTrades         int           `json:"total_trades"`
	WinningTrades       int           `json:"winning_trades"`
	LosingTrades        int           `json:"losing_trades"`
	WinRate             float64       `json:"win_rate"`
	LongestLosingStreak int           `json:"longest_losing_streak"`

	TotalPnL    decimal.Decimal `json:"total_pnl"`     // gross
	TotalPnLPct decimal.Decimal `json:"total_pnl_pct"` // gross
	NetPnL      decimal.Decimal `json:"net_pnl"`
	NetPnLPct   decimal.Decimal `json:"net_pnl_pct"`
	TotalFees   decimal.Decimal `json:"total_fees"`

	MaxDrawdown    decimal.Decimal `json:"max_drawdown"`
	MaxDrawdownPct decimal.Decimal `json:"max_drawdown_pct"`
	SharpeRatio    float64         `json:"sharpe_ratio"`  // on net returns
	SortinoRatio   float64         `json:"sortino_ratio"` // on net returns, downside-only stdev
	FinalEquity    decimal.Decimal `json:"final_equity"`
	StartedAt      time.Time       `json:"started_at"`
	CompletedAt    time.Time       `json:"completed_at"`
	DurationSec    float64         `json:"duration_sec"`
	Error          string          `json:"error,omitempty"`

	// Verdict carries the CRY-10 go/no-go outcome on the net figures.
	Verdict Verdict `json:"verdict"`

	// CacheStats captures how often the AI cache spared us an LLM call —
	// CRY-10 acceptance asks for ≥ 95% hit on warm re-runs.
	CacheStats CacheStats `json:"cache_stats"`
}

// Verdict is the structured PASS/FAIL gate output. Reasons holds each
// failing rule so the CLI can print a punch list rather than a bare "FAIL".
type Verdict struct {
	Pass    bool     `json:"pass"`
	Reasons []string `json:"reasons,omitempty"`
}

// CacheStats reports how the AI cache layer performed on this run.
// Hits = cache returned a stored AnalysisResult.
// Misses = fresh LLM call made (whether or not it landed in the cache).
// Skipped = analyzer errored or returned nil before any signal could be
//   evaluated; these bars produced no trade decision either way.
type CacheStats struct {
	Hits    int `json:"hits"`
	Misses  int `json:"misses"`
	Skipped int `json:"skipped"`
}

// HitRate returns the warm-cache hit ratio as a fraction. Returns 0 when
// no decisions were attempted so the CLI prints "n/a" cleanly.
func (cs CacheStats) HitRate() float64 {
	total := cs.Hits + cs.Misses
	if total == 0 {
		return 0
	}
	return float64(cs.Hits) / float64(total)
}

// Go/no-go thresholds per CRY-10 spec. Hardcoded here (not config) because
// these are the contract — tightening them is a deliberate decision, not a
// per-run knob. Adjust by editing the const if validation history changes.
const (
	gateWinRatePct     = 55.0
	gateMaxDrawdownPct = 15.0
	gateSharpeMinimum  = 1.0
)

// AICache memoises analyzer responses keyed by (symbol, candle open time,
// model). Implementations: DBCache (persistent across runs, used by the CLI)
// and NoopCache (used by the HTTP handler so legacy /api/v1/backtest calls
// don't pollute the cache table).
type AICache interface {
	Get(symbol string, candleOpenTime time.Time, model string) (*ai.AnalysisResult, bool)
	Put(symbol string, candleOpenTime time.Time, model string, result *ai.AnalysisResult)
}

// NoopCache is the default implementation when no cache is wired — every
// call misses, nothing is stored. CacheStats records these as Skipped so
// the operator can tell apart "cache disabled" from "warm cache".
type NoopCache struct{}

func (NoopCache) Get(string, time.Time, string) (*ai.AnalysisResult, bool) { return nil, false }
func (NoopCache) Put(string, time.Time, string, *ai.AnalysisResult)        {}

// Engine replays historical candles through the analyzer.
//
// Risk gates note: only MinExpectedReturnGate and MaxStopLossGate (CRY-37)
// are applied in backtest — pure functions over signal data that behave
// identically to live.
// We deliberately do NOT wire riskmanager.PreTradeCheck here: that helper
// queries the LIVE DB for open positions and daily drawdown, which would
// mix real-system state into a supposedly offline replay. If a future
// caller needs portfolio-level risk gates in backtest, build them against
// the engine's in-memory state, not the live DB.
type Engine struct {
	db       *gorm.DB
	analyzer *strategy.Analyzer
	cache    AICache
}

func NewEngine(db *gorm.DB, analyzer *strategy.Analyzer) *Engine {
	return &Engine{db: db, analyzer: analyzer, cache: NoopCache{}}
}

// WithCache replaces the default NoopCache. Pass a DBCache to persist
// AI results across runs.
func (e *Engine) WithCache(c AICache) *Engine {
	if c != nil {
		e.cache = c
	}
	return e
}

// Run executes a backtest. It loads historical candles from TimescaleDB,
// slides a window across them, and feeds each window to the analyzer.
//
// Look-ahead discipline: at iteration i, the signal is computed from
// window candles[i-W : i] (i.e. candles strictly before i), and the fill
// price is candles[i].Open — the first price a real exchange would have
// offered after the signal-generating candle closed. The previous
// implementation filled at candles[i-1].Close, which was the same bar
// the signal saw — a subtle look-ahead the spec calls out.
func (e *Engine) Run(ctx context.Context, cfg Config) (*Result, error) {
	startedAt := time.Now()

	if cfg.Window == 0 {
		cfg.Window = 100
	}
	if cfg.StepSize == 0 {
		cfg.StepSize = 1
	}
	if cfg.Capital == 0 {
		cfg.Capital = 10000
	}
	if cfg.FeeBps == 0 {
		cfg.FeeBps = 10 // Binance Spot taker, BNB discount off — matches executor.DefaultPaperFeeRate
	}
	if cfg.ConfidenceMin == 0 {
		// PROF-2: mirror the live executor gate (executor.go route/handleSignal
		// admit at confidence >= 0.6). The old 0.7 default certified a stricter
		// bot than the one actually deployed.
		cfg.ConfidenceMin = 0.6
	}
	if cfg.MaxPositionPct == 0 {
		// PROF-6: same default the live riskmanager uses (2%), so a
		// RISK_MAX_POSITION_PCT change can't silently desync the two.
		cfg.MaxPositionPct = riskmanager.DefaultConfig().MaxPositionPct
	}
	if cfg.SlippageBps == 0 {
		// PROF-4: surface the optimism rather than hiding it — fills are
		// exact-price and net PnL will read better than live until the
		// operator supplies a realistic per-leg slippage.
		log.Printf("[backtest] WARN: slippage_bps=0 — fills are slippage-free; net PnL is optimistic vs live. Set Config.SlippageBps to model real fill drift.")
	}

	// Pre-compute the per-leg fee multiplier once so the inner loop just
	// multiplies cost × feeMul. 10 bps = 0.001.
	feeMul := decimal.NewFromFloat(cfg.FeeBps).Div(decimal.NewFromFloat(10000))

	var candles []models.Candle
	q := e.db.WithContext(ctx).
		Where("symbol = ? AND interval = ?", cfg.Symbol, cfg.Interval).
		Order("open_time ASC")
	if !cfg.StartTime.IsZero() {
		q = q.Where("open_time >= ?", cfg.StartTime)
	}
	if !cfg.EndTime.IsZero() {
		q = q.Where("open_time <= ?", cfg.EndTime)
	}
	if err := q.Find(&candles).Error; err != nil {
		return nil, fmt.Errorf("load candles: %w", err)
	}
	// Need enough candles for at least one window plus one next-bar fill.
	if len(candles) < cfg.Window+1 {
		return nil, fmt.Errorf("not enough candles: have %d, need at least %d (window+1)", len(candles), cfg.Window+1)
	}
	log.Printf("[backtest] loaded %d candles for %s/%s", len(candles), cfg.Symbol, cfg.Interval)

	equity := decimal.NewFromFloat(cfg.Capital)
	peak := equity
	maxDrawdown := decimal.Zero
	totalFees := decimal.Zero

	var trades []TradeRecord
	var cacheStats CacheStats

	slipMul := decimal.NewFromFloat(cfg.SlippageBps).Div(decimal.NewFromFloat(10000))
	posPctMul := decimal.NewFromFloat(cfg.MaxPositionPct / 100.0)

	var pos *btPosition // non-nil while holding a long

	// recordClose books a closed trade, settles equity (gross PnL first,
	// then the exit fee — mirroring the live wallet), and folds the realized
	// point into the peak/drawdown curve. Shared by every exit path:
	// deterministic SL/TP/trailing, AI SELL, and end-of-data settle.
	recordClose := func(p *btPosition, exitPrice decimal.Decimal, exitTime time.Time, reason string) {
		exitFee := p.entryQty.Mul(exitPrice).Mul(feeMul)
		grossPnL := exitPrice.Sub(p.entryPrice).Mul(p.entryQty)
		grossPct := decimal.Zero
		if p.entryPrice.IsPositive() {
			grossPct = exitPrice.Sub(p.entryPrice).Div(p.entryPrice).Mul(decimal.NewFromInt(100))
		}
		netPnL := grossPnL.Sub(p.entryFee).Sub(exitFee)
		netPct := decimal.Zero
		entryNotional := p.entryQty.Mul(p.entryPrice)
		if entryNotional.IsPositive() {
			netPct = netPnL.Div(entryNotional).Mul(decimal.NewFromInt(100))
		}
		trades = append(trades, TradeRecord{
			EntryTime:  p.entryTime,
			ExitTime:   exitTime,
			Side:       "BUY",
			EntryPrice: p.entryPrice,
			ExitPrice:  exitPrice,
			Quantity:   p.entryQty,
			EntryFee:   p.entryFee,
			ExitFee:    exitFee,
			PnL:        grossPnL,
			PnLPct:     grossPct,
			NetPnL:     netPnL,
			NetPnLPct:  netPct,
			Confidence: p.entryConfidence,
			Reasoning:  p.entryReasoning,
			ExitReason: reason,
		})
		equity = equity.Add(grossPnL).Sub(exitFee)
		totalFees = totalFees.Add(exitFee)
		if equity.GreaterThan(peak) {
			peak = equity
		}
		if dd := peak.Sub(equity); dd.GreaterThan(maxDrawdown) {
			maxDrawdown = dd
		}
	}

	// Iterate EVERY candle. Deterministic exits (SL/TP/trailing) must be
	// evaluated on every bar a position is held — not just on signal bars —
	// because the live monitor ticks continuously (PROF-1). AI signals are
	// still only evaluated on step bars to preserve the configured cadence.
	// The signal at bar i is computed from candles[i-W : i] (strictly before
	// i) and any fill happens at candles[i].Open — no look-ahead.
	for i := cfg.Window; i < len(candles); i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		cur := candles[i] // the bar we transact on

		// ── (a) Holding: mark-to-market drawdown on this bar's excursion
		// (PROF-5), then replay the live exit hierarchy STOP_LOSS >
		// TRAILING_STOP > TAKE_PROFIT before any AI decision (PROF-1).
		exitedThisBar := false
		if pos != nil {
			markHigh := equity.Add(cur.High.Sub(pos.entryPrice).Mul(pos.entryQty))
			markLow := equity.Add(cur.Low.Sub(pos.entryPrice).Mul(pos.entryQty))
			if markHigh.GreaterThan(peak) {
				peak = markHigh
			}
			if dd := peak.Sub(markLow); dd.GreaterThan(maxDrawdown) {
				maxDrawdown = dd
			}

			if exitPrice, reason, fired := simulateDeterministicExit(
				cur, pos, slipMul, feeMul,
				cfg.TrailingStopPct, cfg.TrailingStopArmPct, cfg.MinLockedProfitPct,
			); fired {
				recordClose(pos, exitPrice, cur.OpenTime, string(reason))
				pos = nil
				exitedThisBar = true
			}
		}

		// ── (b) AI signal cadence: only evaluate on step bars. ──
		if (i-cfg.Window)%cfg.StepSize != 0 {
			continue
		}

		window := candles[i-cfg.Window : i]
		signalBar := candles[i-1] // the bar that just closed; signal is "as of" this bar

		result, _ := e.analyzeWithCache(ctx, cfg, window, signalBar.OpenTime, &cacheStats)
		if result == nil {
			cacheStats.Skipped++
			continue
		}
		sig := result.Signal
		if sig == nil || sig.Confidence < cfg.ConfidenceMin || sig.Action == "HOLD" {
			continue
		}

		switch sig.Action {
		case "BUY":
			if pos != nil || exitedThisBar {
				continue
			}
			// CRY-9 min-return gate — fed the signal-bar Close to match the
			// live executor, which gates on the signal-time current price,
			// not the next-bar fill (PROF-6).
			if blocked, _ := riskmanager.MinExpectedReturnGate(sig.TakeProfit, signalBar.Close.InexactFloat64(), cfg.MinReturnBps); blocked {
				continue
			}
			// CRY-37 stop-distance cap, same signal-bar price as live.
			if blocked, _ := riskmanager.MaxStopLossGate(sig.StopLoss, signalBar.Close.InexactFloat64(), cfg.MaxStopLossPct); blocked {
				continue
			}
			if !cur.Open.IsPositive() {
				continue
			}
			entryPrice := slippedBuy(cur.Open, slipMul)
			if !entryPrice.IsPositive() {
				continue
			}
			qty := equity.Mul(posPctMul).Div(entryPrice).Round(8)
			if !qty.IsPositive() {
				continue
			}
			entryFee := qty.Mul(entryPrice).Mul(feeMul)
			// Fee is paid out of equity immediately so the next BUY compounds
			// on a slightly smaller equity (fee drag).
			equity = equity.Sub(entryFee)
			totalFees = totalFees.Add(entryFee)
			p := &btPosition{
				entryPrice:      entryPrice,
				entryQty:        qty,
				entryFee:        entryFee,
				entryTime:       cur.OpenTime,
				entryConfidence: sig.Confidence,
				entryReasoning:  sig.Reasoning,
				peak:            entryPrice,
			}
			if sig.StopLoss > 0 {
				v := decimal.NewFromFloat(sig.StopLoss)
				p.sl = &v
			}
			if sig.TakeProfit > 0 {
				v := decimal.NewFromFloat(sig.TakeProfit)
				p.tp = &v
			}
			pos = p

		case "SELL":
			if pos == nil || exitedThisBar {
				continue
			}
			recordClose(pos, slippedSell(cur.Open, slipMul), cur.OpenTime, string(models.ExitReasonAISell))
			pos = nil
		}
	}

	// ── Settle a position still open at end-of-data so its (often losing)
	// outcome is not silently dropped from the stats (PROF-3). Mark out at
	// the final candle's Close. ──
	if pos != nil {
		last := candles[len(candles)-1]
		recordClose(pos, slippedSell(last.Close, slipMul), last.CloseTime, exitReasonEndOfData)
		pos = nil
	}

	completedAt := time.Now()

	totalTrades := len(trades)
	winCount := 0
	for _, t := range trades {
		if t.NetPnL.IsPositive() {
			winCount++
		}
	}
	winRate := 0.0
	if totalTrades > 0 {
		winRate = float64(winCount) / float64(totalTrades) * 100
	}

	capitalDec := decimal.NewFromFloat(cfg.Capital)
	grossPnL := decimal.Zero
	for _, t := range trades {
		grossPnL = grossPnL.Add(t.PnL)
	}
	netPnL := equity.Sub(capitalDec) // equity already net of all fees
	grossPct := decimal.Zero
	netPct := decimal.Zero
	if capitalDec.IsPositive() {
		grossPct = grossPnL.Div(capitalDec).Mul(decimal.NewFromInt(100))
		netPct = netPnL.Div(capitalDec).Mul(decimal.NewFromInt(100))
	}
	maxDDPct := decimal.Zero
	if peak.IsPositive() {
		maxDDPct = maxDrawdown.Div(peak).Mul(decimal.NewFromInt(100))
	}

	res := &Result{
		Config:              cfg,
		Trades:              trades,
		TotalTrades:         totalTrades,
		WinningTrades:       winCount,
		LosingTrades:        totalTrades - winCount,
		WinRate:             winRate,
		LongestLosingStreak: longestLosingStreak(trades),
		TotalPnL:            grossPnL,
		TotalPnLPct:         grossPct,
		NetPnL:              netPnL,
		NetPnLPct:           netPct,
		TotalFees:           totalFees,
		MaxDrawdown:         maxDrawdown,
		MaxDrawdownPct:      maxDDPct,
		SharpeRatio:         computeSharpe(trades),
		SortinoRatio:        computeSortino(trades),
		FinalEquity:         equity,
		StartedAt:           startedAt,
		CompletedAt:         completedAt,
		DurationSec:         completedAt.Sub(startedAt).Seconds(),
		CacheStats:          cacheStats,
	}
	res.Verdict = verdictFor(res)
	return res, nil
}

// analyzeWithCache looks up a cached AnalysisResult; on miss it calls the
// analyzer and stores the result. Returns nil on analyzer error so the
// caller can record a Skipped tick without aborting the whole run.
func (e *Engine) analyzeWithCache(
	ctx context.Context,
	cfg Config,
	window []models.Candle,
	signalKey time.Time,
	stats *CacheStats,
) (*ai.AnalysisResult, bool) {
	if cfg.Model != "" {
		if cached, ok := e.cache.Get(cfg.Symbol, signalKey, cfg.Model); ok {
			stats.Hits++
			return cached, true
		}
	}
	r, err := e.analyzer.EvaluateCandles(ctx, window, cfg.Symbol)
	if err != nil {
		return nil, false
	}
	stats.Misses++
	if cfg.Model != "" && r != nil && r.AnalysisResult != nil {
		e.cache.Put(cfg.Symbol, signalKey, cfg.Model, r.AnalysisResult)
	}
	if r == nil {
		return nil, false
	}
	return r.AnalysisResult, false
}

// verdictFor applies the CRY-10 go/no-go thresholds. All four conditions
// must hold for PASS. Reasons enumerate failing rules so the operator gets
// a punch list, not a single boolean.
func verdictFor(r *Result) Verdict {
	var reasons []string
	if r.WinRate < gateWinRatePct {
		reasons = append(reasons, fmt.Sprintf("win_rate %.2f%% < %.0f%%", r.WinRate, gateWinRatePct))
	}
	ddPct, _ := r.MaxDrawdownPct.Float64()
	if ddPct > gateMaxDrawdownPct {
		reasons = append(reasons, fmt.Sprintf("max_drawdown %.2f%% > %.0f%%", ddPct, gateMaxDrawdownPct))
	}
	if r.SharpeRatio < gateSharpeMinimum {
		reasons = append(reasons, fmt.Sprintf("sharpe %.2f < %.1f", r.SharpeRatio, gateSharpeMinimum))
	}
	if !r.NetPnL.IsPositive() {
		reasons = append(reasons, fmt.Sprintf("net_pnl %s ≤ 0", r.NetPnL.StringFixed(2)))
	}
	return Verdict{Pass: len(reasons) == 0, Reasons: reasons}
}

// computeSharpe = mean / stdev of NET per-trade returns (% over entry).
// Risk-free rate assumed 0 (consistent with the pre-CRY-10 helper).
func computeSharpe(trades []TradeRecord) float64 {
	if len(trades) < 2 {
		return 0
	}
	var sum, sumSq float64
	n := float64(len(trades))
	for _, t := range trades {
		r := t.NetPnLPct.InexactFloat64()
		sum += r
		sumSq += r * r
	}
	mean := sum / n
	variance := (sumSq / n) - (mean * mean)
	if variance <= 0 {
		return 0
	}
	stdDev := math.Sqrt(variance)
	if stdDev == 0 {
		return 0
	}
	return mean / stdDev
}

// computeSortino is Sharpe with the denominator restricted to the std-dev
// of negative returns only. Captures downside risk without penalising
// upside volatility — preferred over Sharpe for asymmetric strategies.
//
// If there are no losing trades the denominator collapses to zero and we
// return 0 (matches the Sharpe-style "undefined" convention used here).
func computeSortino(trades []TradeRecord) float64 {
	if len(trades) < 2 {
		return 0
	}
	var meanSum float64
	n := float64(len(trades))
	var downsideSumSq float64
	var downsideN float64
	for _, t := range trades {
		r := t.NetPnLPct.InexactFloat64()
		meanSum += r
		if r < 0 {
			downsideSumSq += r * r
			downsideN++
		}
	}
	mean := meanSum / n
	if downsideN == 0 {
		return 0
	}
	downsideVariance := downsideSumSq / downsideN
	if downsideVariance <= 0 {
		return 0
	}
	return mean / math.Sqrt(downsideVariance)
}

// exitReasonEndOfData marks the synthetic close applied to a position that
// was still open when the backtest window ended (PROF-3).
const exitReasonEndOfData = "END_OF_DATA"

// btPosition is the in-memory state of one open long during replay.
type btPosition struct {
	entryPrice      decimal.Decimal
	entryQty        decimal.Decimal
	entryFee        decimal.Decimal
	entryTime       time.Time
	entryConfidence float64
	entryReasoning  string
	sl, tp          *decimal.Decimal // nil when the AI supplied no level
	peak            decimal.Decimal  // running intrabar peak, for trailing
	trailArmed      bool
}

// slippedBuy / slippedSell apply per-leg slippage adverse to the trade
// direction: a market BUY fills slightly higher, a market SELL slightly
// lower. Mirrors the live executor's fill-drift budget so backtest fills are
// not optimistically exact (PROF-4). slipMul is bps/10000.
func slippedBuy(price, slipMul decimal.Decimal) decimal.Decimal {
	return price.Mul(decimal.NewFromInt(1).Add(slipMul))
}

func slippedSell(price, slipMul decimal.Decimal) decimal.Decimal {
	return price.Mul(decimal.NewFromInt(1).Sub(slipMul))
}

// simulateDeterministicExit replays the live position monitor's exit
// hierarchy (STOP_LOSS > TRAILING_STOP > TAKE_PROFIT) against one OHLC
// candle, updating the trailing peak/armed state in pos. Mirrors
// executor.evaluateCombinedExit + evaluateTrailingStop — keep in sync.
//
// Intrabar fill model: a stop fills at its level (or the candle Open when the
// bar gapped through it); a take-profit fills at its level (or the better
// Open on a gap up); each then takes the adverse slippage leg. When a single
// candle's range spans both the stop and the target, STOP_LOSS wins — the
// conservative convention, and the same precedence the live monitor uses.
func simulateDeterministicExit(
	c models.Candle, pos *btPosition, slipMul, feeMul decimal.Decimal,
	trailPct, armPct, minLockedPct float64,
) (exit decimal.Decimal, reason models.ExitReason, fired bool) {
	if c.High.GreaterThan(pos.peak) {
		pos.peak = c.High
	}

	// 1) STOP_LOSS — price ≤ SL.
	if pos.sl != nil && c.Low.LessThanOrEqual(*pos.sl) {
		base := *pos.sl
		if c.Open.LessThan(base) { // gapped below the stop
			base = c.Open
		}
		return slippedSell(base, slipMul), models.ExitReasonStopLoss, true
	}

	// 2) TRAILING_STOP — only when enabled (trailPct > 0), mirroring the live
	// default where a zero pct disables firing but peak is still tracked.
	if trailPct > 0 && !pos.entryPrice.IsZero() {
		peakPnLPct := pos.peak.Sub(pos.entryPrice).Div(pos.entryPrice).Mul(decimal.NewFromInt(100))
		if pos.trailArmed || peakPnLPct.GreaterThanOrEqual(decimal.NewFromFloat(armPct)) {
			pos.trailArmed = true
			trailThreshold := pos.peak.Mul(decimal.NewFromInt(1).Sub(decimal.NewFromFloat(trailPct).Div(decimal.NewFromInt(100))))
			if c.Low.LessThanOrEqual(trailThreshold) {
				base := trailThreshold
				if c.Open.LessThan(base) {
					base = c.Open
				}
				// net-locked-profit gate (mirrors executor.netLockedPnLPct):
				// only fire when the close locks >= minLockedPct net of fees.
				if netLockedPnLPctBT(pos.entryPrice, base, feeMul).GreaterThanOrEqual(decimal.NewFromFloat(minLockedPct)) {
					return slippedSell(base, slipMul), models.ExitReasonTrailingStop, true
				}
			}
		}
	}

	// 3) TAKE_PROFIT — price ≥ TP.
	if pos.tp != nil && c.High.GreaterThanOrEqual(*pos.tp) {
		base := *pos.tp
		if c.Open.GreaterThan(base) { // gapped above the target
			base = c.Open
		}
		return slippedSell(base, slipMul), models.ExitReasonTakeProfit, true
	}

	return decimal.Zero, "", false
}

// netLockedPnLPctBT mirrors executor.netLockedPnLPct: round-trip net PnL as a
// percent of entry cost (quantity cancels), charging feeMul on each leg.
func netLockedPnLPctBT(entry, exit, feeMul decimal.Decimal) decimal.Decimal {
	if entry.IsZero() {
		return decimal.Zero
	}
	entryFee := entry.Mul(feeMul)
	exitFee := exit.Mul(feeMul)
	net := exit.Sub(entry).Sub(entryFee).Sub(exitFee)
	return net.Div(entry).Mul(decimal.NewFromInt(100))
}

// longestLosingStreak walks the trade log in chronological order and
// returns the longest contiguous run of NetPnL < 0. Zero-PnL trades break
// the streak (treated as neither win nor loss for streak purposes; same
// asymmetric handling as win_rate's `IsPositive`).
func longestLosingStreak(trades []TradeRecord) int {
	longest, current := 0, 0
	for _, t := range trades {
		if t.NetPnL.IsNegative() {
			current++
			if current > longest {
				longest = current
			}
		} else {
			current = 0
		}
	}
	return longest
}
