package ai

import (
	"context"
	"errors"
)

// Provider is the seam between the strategy analyzer and a concrete LLM
// backend. Standard, not a bandaid: the analyzer talks ONLY to this
// interface — swapping Ollama for OpenAI or stacking decorators
// (CircuitBreaker, Retry) is a constructor change, not a refactor.
//
// Implementations MUST be safe for concurrent use.
type Provider interface {
	// Analyze sends MarketContext to the LLM and returns the parsed signal
	// alongside full audit-trail data (Prompt, RawResponse, timing). The
	// audit fields are load-bearing — CRY-5's approval gate persists
	// them so the operator can reconstruct what the LLM "saw" at decision
	// time. Returning a bare Signal would lose that.
	Analyze(ctx context.Context, mc MarketContext) (*AnalysisResult, error)

	// HealthCheck is a lightweight liveness probe — MUST NOT trigger a
	// full LLM inference. The background health monitor calls this every
	// 30s; the production hot path never does. Return value is just
	// "reachable or not" — no payload semantics.
	HealthCheck(ctx context.Context) error

	// Name returns a stable identifier for logging + the /ai/health
	// endpoint. Conventionally lowercase, no spaces ("ollama", "openai").
	Name() string
}

// ErrCircuitOpen is returned by the CircuitBreaker decorator when the
// breaker is in the open state (recent consecutive failures). Callers
// see this WITHOUT a downstream call being made — that's the whole
// point of the breaker: fail fast, don't hammer a known-down backend.
var ErrCircuitOpen = errors.New("ai: circuit breaker open (recent consecutive failures)")
