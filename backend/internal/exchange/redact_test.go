package exchange

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"cryptobot/internal/execution"
)

// A transport failure on a signed GET must not leak the signed query
// (timestamp + signature) into the error text.
func TestDoSigned_TransportErrorRedactsQuery(t *testing.T) {
	c := &BinanceOrderClient{restURL: "http://127.0.0.1:1", http: &http.Client{}}
	_, err := c.doSigned(context.Background(), http.MethodGet, "/api/v3/order",
		"symbol=BTCUSDT&origClientOrderId=exit-1-0&timestamp=1&signature=deadbeef")
	if !errors.Is(err, execution.ErrAmbiguousPlacement) {
		t.Fatalf("err=%v want ErrAmbiguousPlacement", err)
	}
	if msg := err.Error(); strings.Contains(msg, "signature") || strings.Contains(msg, "deadbeef") {
		t.Fatalf("error leaks the signed query: %s", msg)
	}
	if !strings.Contains(err.Error(), "/api/v3/order") {
		t.Fatalf("error should keep the path for debugging: %s", err)
	}
}
