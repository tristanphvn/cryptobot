package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Signal is the structured response the LLM must return.
type Signal struct {
	Action     string  `json:"action"`     // BUY | SELL | HOLD
	Confidence float64 `json:"confidence"` // 0.0 – 1.0
	Reasoning  string  `json:"reasoning"`
	StopLoss   float64 `json:"stop_loss,omitempty"`
	TakeProfit float64 `json:"take_profit,omitempty"`
}

// AnalysisResult bundles the parsed signal with audit trail data.
type AnalysisResult struct {
	Signal      *Signal
	Prompt      string // exact prompt sent to the LLM
	RawResponse string // raw JSON string from the LLM
	SentAt      time.Time
	ReceivedAt  time.Time
}

// MarketContext is the data we send to the LLM for analysis.
type MarketContext struct {
	Symbol       string  `json:"symbol"`
	CurrentPrice float64 `json:"current_price"`
	RSI          float64 `json:"rsi"`
	MACDLine     float64 `json:"macd_line"`
	SignalLine   float64 `json:"signal_line"`
	MACDHist     float64 `json:"macd_histogram"`
	Volume24h    float64 `json:"volume_24h"`
	PriceChange  float64 `json:"price_change_pct"`

	// Position is set by the analyzer when the symbol currently has an
	// open position. nil means "no open position; the model is being
	// asked to consider entry only". CRY-16: the prompt branches on
	// this so the LLM can return a position-aware SELL with discretion.
	Position *PositionContext `json:"position,omitempty"`
}

// PositionContext is the snapshot of an open position handed to the
// LLM so it can reason about whether to maintain, exit, or reverse.
// CRY-16: SL/TP are the immutable risk snapshot taken at OPEN, NOT a
// fresh AI suggestion. CRY-22 adds the peak-tracking fields so the LLM
// can distinguish a healthy pullback in a winner (current price < peak
// but still in profit) from a fresh underwater position — and so it
// knows when the deterministic trailing-stop monitor is armed.
type PositionContext struct {
	EntryPrice          float64 `json:"entry_price"`
	Quantity            float64 `json:"quantity"`
	UnrealizedPnLPct    float64 `json:"unrealized_pnl_pct"`
	HoursHeld           float64 `json:"hours_held"`
	StopLoss            float64 `json:"stop_loss,omitempty"`            // 0 = not set at OPEN
	TakeProfit          float64 `json:"take_profit,omitempty"`          // 0 = not set at OPEN
	PeakPrice           float64 `json:"peak_price,omitempty"`           // 0 = unknown (pre-CRY-22 row, backfill pending)
	DrawdownFromPeakPct float64 `json:"drawdown_from_peak_pct"`         // (peak − current) / peak × 100; 0 when at-or-above peak
	MaxUnrealizedPnLPct float64 `json:"max_unrealized_pnl_pct"`         // best unrealized PnL % seen since open
	TrailingStopArmed   bool    `json:"trailing_stop_armed"`            // true once peak unrealized crossed TRAILING_STOP_ARM_PCT (latched)
}

// OllamaProvider speaks to a local Ollama instance via the /api/generate
// endpoint. Concrete impl of the Provider interface; reliability layers
// (CircuitBreaker, Retry) wrap this without touching prompt construction
// or audit-trail capture.
type OllamaProvider struct {
	host   string
	model  string
	client *http.Client
}

// Compile-time assertion: OllamaProvider satisfies Provider.
var _ Provider = (*OllamaProvider)(nil)

func NewOllamaProvider(host, model string) *OllamaProvider {
	return &OllamaProvider{
		host:  host,
		model: model,
		client: &http.Client{
			Timeout: 120 * time.Second, // LLM inference can be slow on CPU
		},
	}
}

// Name implements Provider — stable identifier surfaced on /ai/health.
func (o *OllamaProvider) Name() string { return "ollama" }

// ollamaRequest matches the Ollama /api/generate payload.
type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
	Format string `json:"format"`
}

type ollamaResponse struct {
	Response string `json:"response"`
}

// Analyze sends market context to the local Ollama instance and returns
// a structured trading signal with full audit trail.
func (o *OllamaProvider) Analyze(ctx context.Context, mkt MarketContext) (*AnalysisResult, error) {
	prompt := buildPrompt(mkt)
	sentAt := time.Now()

	reqBody := ollamaRequest{
		Model:  o.model,
		Prompt: prompt,
		Stream: false,
		Format: "json",
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.host+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama returned %d: %s", resp.StatusCode, string(b))
	}

	var ollamaResp ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&ollamaResp); err != nil {
		return nil, fmt.Errorf("decode ollama response: %w", err)
	}
	receivedAt := time.Now()

	var signal Signal
	if err := json.Unmarshal([]byte(ollamaResp.Response), &signal); err != nil {
		return nil, fmt.Errorf("parse signal JSON: %w (raw: %s)", err, ollamaResp.Response)
	}

	// Validate
	switch signal.Action {
	case "BUY", "SELL", "HOLD":
	default:
		return nil, fmt.Errorf("invalid action %q from LLM", signal.Action)
	}
	if signal.Confidence < 0 || signal.Confidence > 1 {
		return nil, fmt.Errorf("confidence %.4f out of range [0,1]", signal.Confidence)
	}

	return &AnalysisResult{
		Signal:      &signal,
		Prompt:      prompt,
		RawResponse: ollamaResp.Response,
		SentAt:      sentAt,
		ReceivedAt:  receivedAt,
	}, nil
}

// HealthCheck implements Provider — lightweight liveness probe via
// /api/tags. Does NOT trigger LLM inference; safe to call from a 30s
// background loop without burning CPU on the model server.
func (o *OllamaProvider) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.host+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("ollama unreachable: %w", err)
	}
	resp.Body.Close()
	return nil
}

func buildPrompt(mkt MarketContext) string {
	base := fmt.Sprintf(`You are a quantitative crypto trading analyst. Analyze the following market data and return ONLY a JSON object with your trading recommendation.

Market Data for %s:
- Current Price: $%.2f
- RSI (14): %.2f
- MACD Line: %.6f
- Signal Line: %.6f
- MACD Histogram: %.6f
- 24h Volume: $%.2f
- 24h Price Change: %.2f%%

Rules:
- RSI > 70 = overbought (bearish), RSI < 30 = oversold (bullish)
- MACD Line crossing above Signal Line = bullish
- MACD Line crossing below Signal Line = bearish
- Recommend BUY or SELL when confidence >= 0.6.
- Reserve HOLD only for genuinely ambiguous setups (no clear MACD direction AND RSI in the 40-60 neutral band).
- When MACD line is crossing the signal line in the direction your technical read favors, lean toward action over HOLD even at moderate confidence.`,
		mkt.Symbol,
		mkt.CurrentPrice,
		mkt.RSI,
		mkt.MACDLine,
		mkt.SignalLine,
		mkt.MACDHist,
		mkt.Volume24h,
		mkt.PriceChange,
	)

	// CRY-16: position-aware branch. The deterministic monitor handles
	// SL/TP exits as a safety net — the model is free to use discretion
	// over whether the current technical picture justifies maintaining,
	// exiting, or reversing the position. Deliberately NOT prescriptive
	// ("if X then SELL"): the AI keeps its discretionary freedom.
	if mkt.Position != nil {
		base += "\n\n" + buildPositionSection(*mkt.Position)
	}

	base += `

Return ONLY this JSON structure (no markdown, no explanation outside JSON):
{
  "action": "BUY" | "SELL" | "HOLD",
  "confidence": 0.0 to 1.0,
  "reasoning": "brief explanation",
  "stop_loss": suggested stop loss price,
  "take_profit": suggested take profit price
}`
	return base
}

// buildPositionSection formats the open-position context. Pulled out so
// the prompt-builder tests can assert the exact rendered substring
// without re-deriving the indicator section.
//
// CRY-22: when peak data is present the section grows to distinguish
// "we were up X%, now up Y% (drawdown Z%)" from a fresh underwater
// position — the LLM was previously blind to that difference.
func buildPositionSection(p PositionContext) string {
	s := fmt.Sprintf(`You currently hold an open position on this symbol:
- Entry Price: $%.2f
- Quantity: %g
- Unrealized PnL: %+.2f%%
- Held: %.1f hours`,
		p.EntryPrice, p.Quantity, p.UnrealizedPnLPct, p.HoursHeld,
	)
	if p.PeakPrice > 0 {
		s += fmt.Sprintf(
			"\n- Peak Price Since Open: $%.2f (max unrealized %+.2f%%, current drawdown from peak %.2f%%)",
			p.PeakPrice, p.MaxUnrealizedPnLPct, p.DrawdownFromPeakPct,
		)
	}
	if p.TrailingStopArmed {
		s += "\n- Trailing stop is ARMED (deterministic monitor will close on further drawdown from peak; do not assume it will fire — but factor that the safety net exists)"
	}
	if p.StopLoss > 0 || p.TakeProfit > 0 {
		s += "\n- Risk plan at OPEN:"
		if p.StopLoss > 0 {
			s += fmt.Sprintf(" SL=$%.2f", p.StopLoss)
		}
		if p.TakeProfit > 0 {
			s += fmt.Sprintf(" TP=$%.2f", p.TakeProfit)
		}
		s += " (a deterministic monitor enforces these; do not assume they will be hit)"
	}
	s += `

Evaluate whether maintaining, exiting, or reversing the position is justified given the current market data. Return SELL to exit, BUY only if reversing into a fresh long is warranted, HOLD to keep the position open.`
	return s
}
