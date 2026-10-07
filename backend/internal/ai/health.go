package ai

import (
	"context"
	"log"
	"sync"
	"time"
)

// HealthSnapshot is the thread-safe view of provider liveness shown at
// /api/v1/ai/health. Healthy mirrors the most recent HealthCheck result;
// LastCheckAt is when the check ran (UTC); LastError carries the error
// string from the last failure so the operator dashboard can show
// "ollama unreachable: connection refused" instead of just "down".
type HealthSnapshot struct {
	Provider     string    `json:"provider"`
	Healthy      bool      `json:"healthy"`
	LastCheckAt  time.Time `json:"last_check_at"`
	LastError    string    `json:"last_error,omitempty"`
	BreakerState string    `json:"breaker_state,omitempty"`
}

// breakerStater is satisfied by *CircuitBreaker. Used via type assertion
// so HealthMonitor reports breaker state without importing the concrete
// type — keeps the assertion local and optional.
type breakerStater interface {
	State() string
}

// HealthMonitor runs a 30s background loop calling Provider.HealthCheck
// and caches the result. Standard, not a bandaid: callers of the
// /ai/health endpoint never wait on a downstream probe — they read a
// recent snapshot. A flaky Ollama doesn't make the dashboard hang.
type HealthMonitor struct {
	provider Provider
	interval time.Duration

	mu       sync.RWMutex
	snapshot HealthSnapshot
}

// NewHealthMonitor wraps p with a background probe. The first probe
// runs immediately on Start so /ai/health is populated before any
// dashboard reads it.
func NewHealthMonitor(p Provider, interval time.Duration) *HealthMonitor {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &HealthMonitor{
		provider: p,
		interval: interval,
		snapshot: HealthSnapshot{Provider: p.Name()},
	}
}

// Run blocks until ctx is cancelled. Designed to be launched by
// main.go's supervise() helper.
func (h *HealthMonitor) Run(ctx context.Context) {
	h.probe(ctx)
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.probe(ctx)
		}
	}
}

// Snapshot returns the latest cached probe result. Safe for concurrent
// HTTP handlers.
func (h *HealthMonitor) Snapshot() HealthSnapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.snapshot
}

func (h *HealthMonitor) probe(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	err := h.provider.HealthCheck(ctx)

	snap := HealthSnapshot{
		Provider:    h.provider.Name(),
		Healthy:     err == nil,
		LastCheckAt: time.Now().UTC(),
	}
	if err != nil {
		snap.LastError = err.Error()
		log.Printf("[ai/health] %s probe failed: %v", h.provider.Name(), err)
	}
	// Optional breaker state — reported when the provider is wrapped
	// in a CircuitBreaker. Operator can see "closed | open | half-open"
	// alongside the liveness verdict.
	if br, ok := h.provider.(breakerStater); ok {
		snap.BreakerState = br.State()
	}

	h.mu.Lock()
	h.snapshot = snap
	h.mu.Unlock()
}
