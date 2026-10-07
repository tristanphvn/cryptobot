package ai

import "fmt"

// Config carries the inputs the factory needs to construct a Provider.
// Standard, not bandaid: callers pass a single struct rather than ten
// positional args, so adding a future tuneable (api_key, max_tokens, …)
// for OpenAI or Anthropic does not break every existing call site.
type Config struct {
	Provider    string // "ollama" — env AI_PROVIDER, default "ollama"
	OllamaHost  string
	OllamaModel string
}

// NewProvider builds the configured Provider with reliability decorators
// already stacked. Decorator order from outer to inner is:
//
//	CircuitBreaker → Retry → concrete (Ollama)
//
// so a transient failure is retried once silently, and only persistent
// failures count against the breaker. Callers see one Provider; the
// stack is opaque.
//
// An unknown Provider value fails fast — callers should pair this with
// log.Fatal at startup so a typo in env does not silently run with a
// fallback the operator didn't ask for.
func NewProvider(cfg Config) (Provider, error) {
	name := cfg.Provider
	if name == "" {
		name = "ollama"
	}
	var base Provider
	switch name {
	case "ollama":
		base = NewOllamaProvider(cfg.OllamaHost, cfg.OllamaModel)
	default:
		return nil, fmt.Errorf("ai: unknown AI_PROVIDER=%q (supported: ollama)", name)
	}
	// Wrap: retry first, then breaker. See decorator-order note above.
	return NewCircuitBreaker(NewRetrier(base, defaultRetryPolicy), defaultBreakerPolicy), nil
}
