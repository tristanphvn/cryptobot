package ai

import (
	"context"
	"log"
	"sync"
	"time"
)

// BreakerPolicy is the configuration for CircuitBreaker.
//
//	FailureThreshold: how many consecutive failures trip the breaker
//	OpenDuration:     how long the breaker stays open before a half-open trial
type BreakerPolicy struct {
	FailureThreshold int
	OpenDuration     time.Duration
}

// defaultBreakerPolicy: conservative — 3 fails to trip (Ollama can be
// slow on first call when loading the model), 5 min open window (enough
// for a typical Ollama crash + restart cycle).
var defaultBreakerPolicy = BreakerPolicy{
	FailureThreshold: 3,
	OpenDuration:     5 * time.Minute,
}

type breakerState int

const (
	stateClosed   breakerState = iota // calls flow through
	stateOpen                         // calls fail fast with ErrCircuitOpen
	stateHalfOpen                     // one trial call allowed; next result decides
)

// CircuitBreaker wraps a Provider and short-circuits calls when the
// inner backend has been failing recently. Standard, not bandaid: prevents
// the strategy loop from queuing 5-minute LLM timeouts back-to-back
// when Ollama is dead — each tick fails in microseconds with
// ErrCircuitOpen until the open window expires.
//
// HealthCheck passes through unconditionally so the background monitor
// can keep probing while the breaker is open (otherwise we'd lose the
// signal that tells us Ollama is back).
type CircuitBreaker struct {
	inner  Provider
	policy BreakerPolicy

	mu              sync.Mutex
	state           breakerState
	consecutiveFail int
	openedAt        time.Time
}

// NewCircuitBreaker wraps inner with the given policy. Defaults applied
// if policy fields are zero.
func NewCircuitBreaker(inner Provider, policy BreakerPolicy) *CircuitBreaker {
	if policy.FailureThreshold < 1 {
		policy.FailureThreshold = 3
	}
	if policy.OpenDuration <= 0 {
		policy.OpenDuration = 5 * time.Minute
	}
	return &CircuitBreaker{inner: inner, policy: policy, state: stateClosed}
}

var _ Provider = (*CircuitBreaker)(nil)

func (c *CircuitBreaker) Name() string                          { return c.inner.Name() }
func (c *CircuitBreaker) HealthCheck(ctx context.Context) error { return c.inner.HealthCheck(ctx) }

// State exposes the current breaker state as a stable string — used by
// /api/v1/ai/health so the operator dashboard can colour-code.
func (c *CircuitBreaker) State() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maybeTransitionToHalfOpenLocked()
	switch c.state {
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

func (c *CircuitBreaker) Analyze(ctx context.Context, mc MarketContext) (*AnalysisResult, error) {
	c.mu.Lock()
	c.maybeTransitionToHalfOpenLocked()
	if c.state == stateOpen {
		c.mu.Unlock()
		return nil, ErrCircuitOpen
	}
	c.mu.Unlock()

	res, err := c.inner.Analyze(ctx, mc)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.consecutiveFail++
		if c.consecutiveFail >= c.policy.FailureThreshold && c.state != stateOpen {
			c.state = stateOpen
			c.openedAt = time.Now()
			log.Printf("[ai/breaker] %s OPENED after %d consecutive failures — will stay open for %s",
				c.inner.Name(), c.consecutiveFail, c.policy.OpenDuration)
		}
		return nil, err
	}
	if c.state == stateHalfOpen {
		log.Printf("[ai/breaker] %s recovered — half-open trial succeeded, closing", c.inner.Name())
	}
	c.consecutiveFail = 0
	c.state = stateClosed
	return res, nil
}

// maybeTransitionToHalfOpenLocked moves the breaker from Open to HalfOpen
// once OpenDuration has elapsed since openedAt. Caller MUST hold c.mu.
func (c *CircuitBreaker) maybeTransitionToHalfOpenLocked() {
	if c.state == stateOpen && time.Since(c.openedAt) >= c.policy.OpenDuration {
		c.state = stateHalfOpen
		log.Printf("[ai/breaker] %s open window elapsed — entering half-open (1 trial call)", c.inner.Name())
	}
}
