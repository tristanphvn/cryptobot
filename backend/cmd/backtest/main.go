// Command backtest replays AI signals over historical candle data and
// produces a go/no-go report — the CRY-10 gate before paper-trade
// validation (CRY-12) and live trading (CRY-13).
//
// Each symbol is run through internal/backtest.Engine with the same
// MinExpectedReturnGate the live executor enforces (extracted into
// riskmanager so the two paths can't drift). AI calls hit a persistent
// cache (backtest_ai_cache table) keyed on (symbol, candle_open_time,
// model) so re-running with a tuned fee or capital figure does not
// require fresh LLM inference.
//
// Usage:
//
//	go run ./cmd/backtest \
//	    --symbols=BTCUSDT,ETHUSDT \
//	    --from=2025-05-01 \
//	    --to=2026-05-01 \
//	    --fee-bps=10 \
//	    --initial-capital=10000
//
// Per-symbol output:
//
//	backtest_results/<symbol>-<YYYY-MM-DD>.md   — human-readable report
//	backtest_results/<symbol>-<YYYY-MM-DD>.json — full structured Result
//	backtest_results/<symbol>-<YYYY-MM-DD>.csv  — trade log
//
// Exit code is 0 when EVERY symbol passes the go/no-go gate, else 1.
package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cryptobot/internal/ai"
	"cryptobot/internal/backtest"
	"cryptobot/internal/config"
	"cryptobot/internal/database"
	"cryptobot/internal/strategy"
)

const reportDir = "backtest_results"

func main() {
	var (
		symbolsFlag    = flag.String("symbols", "BTCUSDT", "comma-separated symbols")
		fromFlag       = flag.String("from", "", "start date YYYY-MM-DD UTC (default: 12 months back)")
		toFlag         = flag.String("to", "", "end date YYYY-MM-DD UTC (default: today)")
		intervalFlag   = flag.String("interval", "1h", "candle interval")
		feeBpsFlag     = flag.Float64("fee-bps", 10, "per-leg taker fee in basis points")
		capitalFlag    = flag.Float64("initial-capital", 10000, "starting equity USD")
		windowFlag     = flag.Int("window", 100, "lookback candles per analyzer call")
		confidenceFlag = flag.Float64("confidence-min", 0.6, "ignore signals below this confidence (default 0.6 = live executor gate)")
		slippageFlag   = flag.Float64("slippage-bps", 0, "per-leg slippage in bps applied adverse to fills; 0 = optimistic, set to model real fill drift")
	)
	flag.Parse()

	symbols := splitSymbols(*symbolsFlag)
	if len(symbols) == 0 {
		log.Fatalf("--symbols is required")
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	if err := database.Migrate(db); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	// The CLI uses the same AI factory stack as the server (CircuitBreaker
	// → Retry → Ollama) so cached responses come from the exact provider
	// the live trader would have called.
	provider, err := ai.NewProvider(ai.Config{
		Provider:    cfg.AIProvider,
		OllamaHost:  cfg.OllamaHost,
		OllamaModel: cfg.OllamaModel,
	})
	if err != nil {
		log.Fatalf("ai provider: %v", err)
	}
	analyzer := strategy.NewAnalyzer(db, provider)
	cache := backtest.NewDBCache(db)

	from, to := resolveRange(*fromFlag, *toFlag)
	log.Printf("backtest range: %s → %s  symbols=%v  fee=%.2fbps  capital=%.0f  model=%s",
		from.Format("2006-01-02"), to.Format("2006-01-02"),
		symbols, *feeBpsFlag, *capitalFlag, cfg.OllamaModel)

	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", reportDir, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	engine := backtest.NewEngine(db, analyzer).WithCache(cache)

	allPass := true
	for _, sym := range symbols {
		runCfg := backtest.Config{
			Symbol:         sym,
			Interval:       *intervalFlag,
			StartTime:      from,
			EndTime:        to,
			Capital:        *capitalFlag,
			Window:         *windowFlag,
			StepSize:       1,
			FeeBps:         *feeBpsFlag,
			MinReturnBps:   cfg.MinExpectedReturnBps,
			MaxStopLossPct: cfg.MaxStopLossPct,
			Model:          cfg.OllamaModel,
			ConfidenceMin:  *confidenceFlag,
			// Mirror the live executor's risk/exit config so the backtest
			// replays the same economics (PROF-1/4/6).
			SlippageBps:        *slippageFlag,
			MaxPositionPct:     cfg.RiskMaxPositionPct,
			TrailingStopPct:    cfg.TrailingStopPct,
			TrailingStopArmPct: cfg.TrailingStopArmPct,
			MinLockedProfitPct: cfg.MinLockedProfitPct,
		}
		log.Printf("─── %s ───", sym)
		result, err := engine.Run(ctx, runCfg)
		if err != nil {
			log.Printf("[%s] FAIL: %v", sym, err)
			allPass = false
			continue
		}
		writeReports(reportDir, sym, result)
		printVerdict(sym, result)
		if !result.Verdict.Pass {
			allPass = false
		}
	}

	if !allPass {
		os.Exit(1)
	}
}

// splitSymbols normalises the --symbols flag: trims whitespace, drops
// empty entries, uppercases each.
func splitSymbols(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := strings.TrimSpace(p)
		if s == "" {
			continue
		}
		out = append(out, strings.ToUpper(s))
	}
	return out
}

// resolveRange parses --from / --to. Empty values default to "12 months
// back from now" / "now", matching the CRY-10 acceptance default. Dates
// are interpreted as UTC midnight.
func resolveRange(fromS, toS string) (time.Time, time.Time) {
	now := time.Now().UTC()
	defaultEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	defaultStart := defaultEnd.AddDate(-1, 0, 0)
	parse := func(s string) time.Time {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			log.Fatalf("date %q: %v (expected YYYY-MM-DD)", s, err)
		}
		return t.UTC()
	}
	from := defaultStart
	to := defaultEnd
	if fromS != "" {
		from = parse(fromS)
	}
	if toS != "" {
		to = parse(toS)
	}
	if !to.After(from) {
		log.Fatalf("--to %s must be after --from %s", to.Format("2006-01-02"), from.Format("2006-01-02"))
	}
	return from, to
}

// printVerdict writes a short PASS/FAIL line to stdout so a CI invocation
// can grep the log without parsing JSON. The detailed reasons live in the
// markdown report.
func printVerdict(symbol string, r *backtest.Result) {
	tag := "PASS"
	if !r.Verdict.Pass {
		tag = "FAIL"
	}
	log.Printf("[%s] %s | trades=%d win=%.1f%% sharpe=%.2f sortino=%.2f netPnL=%s drawdown=%s%% cache=%.0f%%",
		symbol, tag,
		r.TotalTrades, r.WinRate, r.SharpeRatio, r.SortinoRatio,
		r.NetPnL.StringFixed(2), r.MaxDrawdownPct.StringFixed(2),
		r.CacheStats.HitRate()*100,
	)
	if !r.Verdict.Pass {
		for _, reason := range r.Verdict.Reasons {
			log.Printf("[%s]   reason: %s", symbol, reason)
		}
	}
}

// writeReports persists the three output formats: markdown (human read),
// JSON (full Result struct), CSV (trade log for spreadsheet analysis).
// Failures are logged but do not abort the run — losing a write is worse
// than losing the report file alone.
func writeReports(dir, symbol string, r *backtest.Result) {
	stamp := time.Now().UTC().Format("2006-01-02")
	base := filepath.Join(dir, fmt.Sprintf("%s-%s", symbol, stamp))

	if err := os.WriteFile(base+".md", []byte(renderMarkdown(symbol, r)), 0o644); err != nil {
		log.Printf("[%s] write markdown: %v", symbol, err)
	}
	if buf, err := json.MarshalIndent(r, "", "  "); err == nil {
		if err := os.WriteFile(base+".json", buf, 0o644); err != nil {
			log.Printf("[%s] write json: %v", symbol, err)
		}
	}
	if err := writeCSV(base+".csv", r); err != nil {
		log.Printf("[%s] write csv: %v", symbol, err)
	}
}

// writeCSV emits the trade log in a spreadsheet-friendly shape: one row
// per trade, columns chronological + entry/exit prices + fees + PnL.
func writeCSV(path string, r *backtest.Result) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	if err := w.Write([]string{
		"entry_time", "exit_time", "side",
		"entry_price", "exit_price", "quantity",
		"entry_fee", "exit_fee",
		"gross_pnl", "gross_pnl_pct",
		"net_pnl", "net_pnl_pct",
		"confidence", "reasoning",
	}); err != nil {
		return err
	}
	for _, t := range r.Trades {
		if err := w.Write([]string{
			t.EntryTime.Format(time.RFC3339),
			t.ExitTime.Format(time.RFC3339),
			t.Side,
			t.EntryPrice.String(),
			t.ExitPrice.String(),
			t.Quantity.String(),
			t.EntryFee.String(),
			t.ExitFee.String(),
			t.PnL.String(),
			t.PnLPct.String(),
			t.NetPnL.String(),
			t.NetPnLPct.String(),
			fmt.Sprintf("%.4f", t.Confidence),
			t.Reasoning,
		}); err != nil {
			return err
		}
	}
	return nil
}

// renderMarkdown builds the human-readable report. We bias toward
// metrics the operator looks at first (verdict, win-rate, drawdown,
// fee drag); the JSON file carries everything else.
func renderMarkdown(symbol string, r *backtest.Result) string {
	var b strings.Builder
	verdict := "PASS"
	if !r.Verdict.Pass {
		verdict = "FAIL"
	}

	fmt.Fprintf(&b, "# Backtest report — %s\n\n", symbol)
	fmt.Fprintf(&b, "**Verdict:** %s  \n", verdict)
	fmt.Fprintf(&b, "**Range:** %s → %s (%s candles)  \n",
		r.Config.StartTime.Format("2006-01-02"),
		r.Config.EndTime.Format("2006-01-02"),
		r.Config.Interval)
	fmt.Fprintf(&b, "**Model:** %s  \n", r.Config.Model)
	fmt.Fprintf(&b, "**Capital:** $%.2f  | **Fee:** %.2f bps/leg  | **Min-return gate:** %d bps  \n",
		r.Config.Capital, r.Config.FeeBps, r.Config.MinReturnBps)
	fmt.Fprintf(&b, "**Run duration:** %.1f s  | **Cache hit rate:** %.1f%% (%d/%d)\n\n",
		r.DurationSec, r.CacheStats.HitRate()*100,
		r.CacheStats.Hits, r.CacheStats.Hits+r.CacheStats.Misses)

	if !r.Verdict.Pass {
		b.WriteString("## Why it failed\n\n")
		for _, reason := range r.Verdict.Reasons {
			fmt.Fprintf(&b, "- %s\n", reason)
		}
		b.WriteString("\n")
	}

	b.WriteString("## Headline metrics\n\n")
	b.WriteString("| Metric | Value |\n|---|---|\n")
	fmt.Fprintf(&b, "| Total trades | %d |\n", r.TotalTrades)
	fmt.Fprintf(&b, "| Win rate | %.2f%% |\n", r.WinRate)
	fmt.Fprintf(&b, "| Winning / losing | %d / %d |\n", r.WinningTrades, r.LosingTrades)
	fmt.Fprintf(&b, "| Longest losing streak | %d |\n", r.LongestLosingStreak)
	fmt.Fprintf(&b, "| Gross PnL | $%s (%s%%) |\n", r.TotalPnL.StringFixed(2), r.TotalPnLPct.StringFixed(2))
	fmt.Fprintf(&b, "| **Net PnL** | **$%s (%s%%)** |\n", r.NetPnL.StringFixed(2), r.NetPnLPct.StringFixed(2))
	fmt.Fprintf(&b, "| Total fees paid | $%s |\n", r.TotalFees.StringFixed(2))
	fmt.Fprintf(&b, "| Max drawdown | $%s (%s%%) |\n", r.MaxDrawdown.StringFixed(2), r.MaxDrawdownPct.StringFixed(2))
	fmt.Fprintf(&b, "| Sharpe ratio | %.3f |\n", r.SharpeRatio)
	fmt.Fprintf(&b, "| Sortino ratio | %.3f |\n", r.SortinoRatio)
	fmt.Fprintf(&b, "| Final equity | $%s |\n", r.FinalEquity.StringFixed(2))
	b.WriteString("\n")

	b.WriteString("## Go/no-go thresholds (CRY-10)\n\n")
	b.WriteString("| Rule | Threshold | Result | |\n|---|---|---|---|\n")
	fmt.Fprintf(&b, "| Win rate | ≥ 55%% | %.2f%% | %s |\n", r.WinRate, passFail(r.WinRate >= 55.0))
	ddPct, _ := r.MaxDrawdownPct.Float64()
	fmt.Fprintf(&b, "| Max drawdown | ≤ 15%% | %.2f%% | %s |\n", ddPct, passFail(ddPct <= 15.0))
	fmt.Fprintf(&b, "| Sharpe | ≥ 1.0 | %.2f | %s |\n", r.SharpeRatio, passFail(r.SharpeRatio >= 1.0))
	fmt.Fprintf(&b, "| Net PnL | > 0 | $%s | %s |\n", r.NetPnL.StringFixed(2), passFail(r.NetPnL.IsPositive()))
	b.WriteString("\n")

	b.WriteString("## Trade log preview\n\n")
	if len(r.Trades) == 0 {
		b.WriteString("_No trades executed._\n")
	} else {
		preview := r.Trades
		if len(preview) > 10 {
			preview = preview[:10]
			fmt.Fprintf(&b, "_First 10 of %d trades — full log in the .csv file._\n\n", len(r.Trades))
		}
		b.WriteString("| Entry | Exit | Entry $ | Exit $ | Net PnL | Net % |\n|---|---|---|---|---|---|\n")
		for _, t := range preview {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | $%s | %s%% |\n",
				t.EntryTime.Format("2006-01-02 15:04"),
				t.ExitTime.Format("2006-01-02 15:04"),
				t.EntryPrice.StringFixed(2),
				t.ExitPrice.StringFixed(2),
				t.NetPnL.StringFixed(2),
				t.NetPnLPct.StringFixed(2),
			)
		}
	}
	return b.String()
}

func passFail(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}
