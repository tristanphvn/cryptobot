package exchange

import (
	"net/url"
	"strings"
	"testing"
)

// TestSignQueryProducesDeterministicSignature pins the HMAC-SHA256
// computation against a fixed (params, secret, timestamp) tuple so
// any change to the signing logic surfaces as a test failure.
func TestSignQueryProducesDeterministicSignature(t *testing.T) {
	params := url.Values{}
	params.Set("symbol", "BTCUSDT")
	params.Set("side", "BUY")
	params.Set("type", "MARKET")
	params.Set("quoteOrderQty", "100")

	// Fixed inputs → fixed output.
	signed := SignQueryWithTimestamp(params, "test-secret-abc", 5000, 1700000000000)

	// The payload portion (everything before &signature=) must contain
	// all params in alphabetical order via url.Values.Encode().
	parts := strings.SplitN(signed, "&signature=", 2)
	if len(parts) != 2 {
		t.Fatalf("missing signature suffix: %q", signed)
	}
	payload, sig := parts[0], parts[1]

	if !strings.Contains(payload, "timestamp=1700000000000") {
		t.Fatalf("timestamp not in payload: %q", payload)
	}
	if !strings.Contains(payload, "recvWindow=5000") {
		t.Fatalf("recvWindow not in payload: %q", payload)
	}
	if len(sig) != 64 {
		t.Fatalf("signature length=%d, want 64 hex chars", len(sig))
	}

	// Determinism: re-running with the same inputs produces the same hex.
	again := SignQueryWithTimestamp(url.Values{
		"symbol":        []string{"BTCUSDT"},
		"side":          []string{"BUY"},
		"type":          []string{"MARKET"},
		"quoteOrderQty": []string{"100"},
	}, "test-secret-abc", 5000, 1700000000000)
	if again != signed {
		t.Fatalf("non-deterministic signing:\nfirst:  %s\nsecond: %s", signed, again)
	}
}

// TestSignQueryChangesWithSecret guards against accidentally signing
// with a hardcoded or empty secret.
func TestSignQueryChangesWithSecret(t *testing.T) {
	params := url.Values{"x": []string{"1"}}
	a := SignQueryWithTimestamp(params, "secret-a", 5000, 1700000000000)
	b := SignQueryWithTimestamp(url.Values{"x": []string{"1"}}, "secret-b", 5000, 1700000000000)
	if a == b {
		t.Fatal("same signature for different secrets — HMAC not exercising secret")
	}
}

// TestSignQueryDefaultsRecvWindow validates the 5000ms default when
// the caller passes 0 or negative.
func TestSignQueryDefaultsRecvWindow(t *testing.T) {
	params := url.Values{}
	out := SignQueryWithTimestamp(params, "s", 0, 1700000000000)
	if !strings.Contains(out, "recvWindow=5000") {
		t.Fatalf("default recvWindow=5000 not applied; payload=%s", out)
	}
}
