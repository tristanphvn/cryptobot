package ai

import (
	"context"
	"errors"
	"log"
	"net"
	"net/url"
	"strings"
	"time"
)

// RetryPolicy is what the Retrier consults to decide whether and how to
// retry a failed Analyze call. Zero MaxAttempts disables retry; passes
// through on first failure.
type RetryPolicy struct {
	MaxAttempts int           // total attempts including the initial call (2 = 1 retry)
	Delay       time.Duration // sleep between attempts
}

var defaultRetryPolicy = RetryPolicy{
	MaxAttempts: 2,
	Delay:       500 * time.Millisecond,
}

// Retrier wraps a Provider with a single bounded retry on transient
// errors (timeout, 5xx). Non-transient errors (bad JSON, validation,
// signature) skip the retry — they will not be cured by trying again.
//
// HealthCheck and Name pass through directly; retrying a liveness probe
// would only delay an open-circuit decision.
type Retrier struct {
	inner  Provider
	policy RetryPolicy
}

// NewRetrier wraps inner with the given policy.
func NewRetrier(inner Provider, policy RetryPolicy) *Retrier {
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 1
	}
	return &Retrier{inner: inner, policy: policy}
}

var _ Provider = (*Retrier)(nil)

func (r *Retrier) Name() string                              { return r.inner.Name() }
func (r *Retrier) HealthCheck(ctx context.Context) error     { return r.inner.HealthCheck(ctx) }

func (r *Retrier) Analyze(ctx context.Context, mc MarketContext) (*AnalysisResult, error) {
	var lastErr error
	for attempt := 1; attempt <= r.policy.MaxAttempts; attempt++ {
		res, err := r.inner.Analyze(ctx, mc)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !isTransient(err) || attempt == r.policy.MaxAttempts {
			return nil, err
		}
		log.Printf("[ai/retry] %s attempt %d/%d failed (%v) — retrying in %s",
			r.inner.Name(), attempt, r.policy.MaxAttempts, err, r.policy.Delay)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(r.policy.Delay):
		}
	}
	return nil, lastErr
}

// isTransient classifies whether an error is worth a retry. Conservative:
// only timeouts, network failures, and 5xx-shaped errors qualify.
// Parse / validation / signature errors are deterministic — retrying
// them just delays the failure.
func isTransient(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Ollama wraps non-2xx as "ollama returned %d: %s" — surface 5xx as
	// transient. Anything 4xx is a deterministic client error.
	msg := err.Error()
	for _, code := range []string{"500", "502", "503", "504"} {
		if strings.Contains(msg, "returned "+code) {
			return true
		}
	}
	return false
}
