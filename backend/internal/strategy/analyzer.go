package strategy

import (
	"context"
	"fmt"
	"time"

	"cryptobot/internal/ai"
	"cryptobot/internal/database/models"
	"cryptobot/pkg/indicators"

	"gorm.io/gorm"
)

// Analyzer computes indicators and queries the LLM for a signal.
//
// CRY-7: holds an ai.Provider (interface), not a concrete OllamaClient.
// main.go stacks CircuitBreaker → Retry → Ollama via ai.NewProvider, so
// the analyzer gets reliability for free without any switch-on-mode here.
type Analyzer struct {
	db       *gorm.DB
	provider ai.Provider
}

func NewAnalyzer(db *gorm.DB, provider ai.Provider) *Analyzer {
	return &Analyzer{db: db, provider: provider}
}

// EvalResult bundles the AI analysis result with the market context used.
type EvalResult struct {
	*ai.AnalysisResult
	Market ai.MarketContext
}

// Evaluate fetches the latest candles, computes RSI/MACD, attaches any
// open-position context (CRY-16), and asks the LLM.
func (a *Analyzer) Evaluate(ctx context.Context, symbol, interval string) (*EvalResult, error) {
	candles, err := a.fetchCandles(ctx, symbol, interval, 100)
	if err != nil {
		return nil, err
	}

	mkt, err := a.buildMarketContext(candles, symbol)
	if err != nil {
		return nil, err
	}

	// CRY-16: hand the LLM the open-position snapshot so it can return
	// a position-aware SELL with discretion. Best-effort: a DB hiccup
	// degrades to entry-only analysis rather than failing the tick.
	if pos := a.fetchOpenPositionContext(ctx, symbol, mkt.CurrentPrice); pos != nil {
		mkt.Position = pos
	}

	result, err := a.provider.Analyze(ctx, mkt)
	if err != nil {
		return nil, err
	}

	return &EvalResult{AnalysisResult: result, Market: mkt}, nil
}

// fetchOpenPositionContext returns an ai.PositionContext describing the
// currently OPEN position on symbol, or nil when there is none. SL/TP
// here are the immutable snapshot from OPEN — NEVER fresh AI output.
func (a *Analyzer) fetchOpenPositionContext(ctx context.Context, symbol string, currentPrice float64) *ai.PositionContext {
	var pos models.Position
	err := a.db.WithContext(ctx).
		Where("symbol = ? AND is_open = ?", symbol, true).
		First(&pos).Error
	if err != nil {
		return nil
	}
	entry, _ := pos.AvgEntryPrice.Float64()
	qty, _ := pos.Quantity.Float64()
	pc := &ai.PositionContext{
		EntryPrice:        entry,
		Quantity:          qty,
		HoursHeld:         time.Since(pos.CreatedAt).Hours(),
		TrailingStopArmed: pos.TrailingStopArmed,
	}
	if entry > 0 {
		pc.UnrealizedPnLPct = (currentPrice - entry) / entry * 100
	}
	if pos.StopLoss != nil {
		pc.StopLoss, _ = pos.StopLoss.Float64()
	}
	if pos.TakeProfit != nil {
		pc.TakeProfit, _ = pos.TakeProfit.Float64()
	}
	// CRY-22 peak-tracking fields. PeakPrice is nullable on the row
	// (pre-CRY-22 positions before BackfillPositionPeaks ran, or a
	// brand-new position the monitor hasn't ticked yet). Leave PeakPrice
	// = 0 so buildPositionSection skips the line cleanly.
	if pos.PeakPrice != nil {
		peak, _ := pos.PeakPrice.Float64()
		pc.PeakPrice = peak
		if peak > 0 && currentPrice < peak {
			pc.DrawdownFromPeakPct = (peak - currentPrice) / peak * 100
		}
	}
	if pos.MaxUnrealizedPnLPct != nil {
		pc.MaxUnrealizedPnLPct, _ = pos.MaxUnrealizedPnLPct.Float64()
	}
	return pc
}

// EvaluateCandles is used by the backtesting engine — it takes pre-loaded candles
// instead of querying the database.
func (a *Analyzer) EvaluateCandles(ctx context.Context, candles []models.Candle, symbol string) (*EvalResult, error) {
	if len(candles) < 30 {
		return nil, fmt.Errorf("not enough candle data (%d), need at least 30", len(candles))
	}

	mkt, err := a.buildMarketContext(candles, symbol)
	if err != nil {
		return nil, err
	}

	result, err := a.provider.Analyze(ctx, mkt)
	if err != nil {
		return nil, err
	}

	return &EvalResult{AnalysisResult: result, Market: mkt}, nil
}

func (a *Analyzer) fetchCandles(ctx context.Context, symbol, interval string, limit int) ([]models.Candle, error) {
	var candles []models.Candle
	result := a.db.WithContext(ctx).
		Where("symbol = ? AND interval = ?", symbol, interval).
		Order("open_time DESC").
		Limit(limit).
		Find(&candles)
	if result.Error != nil {
		return nil, fmt.Errorf("fetch candles: %w", result.Error)
	}
	if len(candles) < 30 {
		return nil, fmt.Errorf("not enough candle data (%d), need at least 30", len(candles))
	}

	// Reverse so index 0 = oldest
	for i, j := 0, len(candles)-1; i < j; i, j = i+1, j-1 {
		candles[i], candles[j] = candles[j], candles[i]
	}
	return candles, nil
}

func (a *Analyzer) buildMarketContext(candles []models.Candle, symbol string) (ai.MarketContext, error) {
	closes := make([]float64, len(candles))
	for i, c := range candles {
		closes[i], _ = c.Close.Float64()
	}

	// Indicators
	rsiValues := indicators.RSI(closes, 14)
	macdValues := indicators.MACD(closes, 12, 26, 9)

	if len(rsiValues) == 0 || len(macdValues) == 0 {
		return ai.MarketContext{}, fmt.Errorf("not enough data to compute indicators")
	}

	latestRSI := rsiValues[len(rsiValues)-1]
	latestMACD := macdValues[len(macdValues)-1]

	lastCandle := candles[len(candles)-1]
	currentPrice, _ := lastCandle.Close.Float64()
	volume, _ := lastCandle.Volume.Float64()

	// Price change: (latest close - close 24 candles ago) / close 24 candles ago * 100
	var priceChange float64
	if len(candles) > 24 {
		prev, _ := candles[len(candles)-25].Close.Float64()
		if prev > 0 {
			priceChange = (currentPrice - prev) / prev * 100
		}
	}

	return ai.MarketContext{
		Symbol:       symbol,
		CurrentPrice: currentPrice,
		RSI:          latestRSI,
		MACDLine:     latestMACD.MACDLine,
		SignalLine:   latestMACD.SignalLine,
		MACDHist:     latestMACD.Histogram,
		Volume24h:    volume,
		PriceChange:  priceChange,
	}, nil
}
