package ai

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProvider is a programmable Provider for decorator tests. Each
// call advances the script index; callers shape the success/failure
// sequence via err values.
type fakeProvider struct {
	name     string
	calls    int32
	script   []error // one entry per expected Analyze call
	healthOK bool
}

func (f *fakeProvider) Name() string { return f.name }

func (f *fakeProvider) HealthCheck(_ context.Context) error {
	if f.healthOK {
		return nil
	}
	return errors.New("health: down")
}

func (f *fakeProvider) Analyze(_ context.Context, _ MarketContext) (*AnalysisResult, error) {
	idx := atomic.AddInt32(&f.calls, 1) - 1
	if int(idx) >= len(f.script) {
		// Past the script — return last entry to simulate steady state.
		idx = int32(len(f.script) - 1)
	}
	if err := f.script[idx]; err != nil {
		return nil, err
	}
	return &AnalysisResult{Signal: &Signal{Action: "HOLD", Confidence: 0.5}}, nil
}

func TestCircuitBreaker_OpensAfterConsecutiveFailures(t *testing.T) {
	fake := &fakeProvider{
		name:   "ollama",
		script: []error{errors.New("boom"), errors.New("boom"), errors.New("boom"), nil},
	}
	cb := NewCircuitBreaker(fake, BreakerPolicy{FailureThreshold: 3, OpenDuration: 50 * time.Millisecond})

	for i := 1; i <= 3; i++ {
		if _, err := cb.Analyze(context.Background(), MarketContext{}); err == nil {
			t.Fatalf("attempt %d: expected error", i)
		}
	}
	if state := cb.State(); state != "open" {
		t.Fatalf("state=%s want open after 3 fails", state)
	}
	// Next call must short-circuit with ErrCircuitOpen, NOT reach inner.
	callsBefore := atomic.LoadInt32(&fake.calls)
	_, err := cb.Analyze(context.Background(), MarketContext{})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("got %v want ErrCircuitOpen", err)
	}
	if atomic.LoadInt32(&fake.calls) != callsBefore {
		t.Fatal("inner was called while circuit open — defeats the point of the breaker")
	}
}

func TestCircuitBreaker_HalfOpenTrialRecovers(t *testing.T) {
	fake := &fakeProvider{
		name:   "ollama",
		script: []error{errors.New("boom"), errors.New("boom"), errors.New("boom"), nil},
	}
	cb := NewCircuitBreaker(fake, BreakerPolicy{FailureThreshold: 3, OpenDuration: 20 * time.Millisecond})

	for i := 0; i < 3; i++ {
		_, _ = cb.Analyze(context.Background(), MarketContext{})
	}
	if cb.State() != "open" {
		t.Fatal("breaker should be open")
	}
	time.Sleep(25 * time.Millisecond) // past OpenDuration

	// Half-open trial — inner returns success → breaker closes.
	if _, err := cb.Analyze(context.Background(), MarketContext{}); err != nil {
		t.Fatalf("half-open trial failed: %v", err)
	}
	if state := cb.State(); state != "closed" {
		t.Fatalf("state=%s want closed after successful trial", state)
	}
}

func TestRetrier_TransientTimeoutRetried(t *testing.T) {
	fake := &fakeProvider{
		name:   "ollama",
		script: []error{context.DeadlineExceeded, nil},
	}
	r := NewRetrier(fake, RetryPolicy{MaxAttempts: 2, Delay: 1 * time.Millisecond})

	res, err := r.Analyze(context.Background(), MarketContext{})
	if err != nil {
		t.Fatalf("expected retry to recover, got %v", err)
	}
	if res == nil {
		t.Fatal("nil result on retry success")
	}
	if got := atomic.LoadInt32(&fake.calls); got != 2 {
		t.Fatalf("inner calls=%d want 2 (1 fail + 1 retry)", got)
	}
}

func TestRetrier_DeterministicErrorNotRetried(t *testing.T) {
	// Bad JSON / validation errors are not transient — retry just delays the failure.
	fake := &fakeProvider{
		name:   "ollama",
		script: []error{errors.New("parse signal JSON: unexpected token"), nil},
	}
	r := NewRetrier(fake, RetryPolicy{MaxAttempts: 3, Delay: 1 * time.Millisecond})

	if _, err := r.Analyze(context.Background(), MarketContext{}); err == nil {
		t.Fatal("expected deterministic error to surface")
	}
	if got := atomic.LoadInt32(&fake.calls); got != 1 {
		t.Fatalf("inner calls=%d want 1 (no retry on deterministic error)", got)
	}
}

func TestRetrier_5xxRetried(t *testing.T) {
	fake := &fakeProvider{
		name:   "ollama",
		script: []error{errors.New("ollama returned 503: gateway"), nil},
	}
	r := NewRetrier(fake, RetryPolicy{MaxAttempts: 2, Delay: 1 * time.Millisecond})

	if _, err := r.Analyze(context.Background(), MarketContext{}); err != nil {
		t.Fatalf("expected 503 to be retried successfully: %v", err)
	}
}

func TestNewProvider_UnknownFailsFast(t *testing.T) {
	_, err := NewProvider(Config{Provider: "openai"})
	if err == nil {
		t.Fatal("expected unknown provider to error")
	}
}

func TestNewProvider_DefaultsToOllama(t *testing.T) {
	p, err := NewProvider(Config{OllamaHost: "http://localhost:11434", OllamaModel: "llama3"})
	if err != nil {
		t.Fatalf("default ollama provider should construct: %v", err)
	}
	if p.Name() != "ollama" {
		t.Fatalf("name=%s want ollama", p.Name())
	}
}
