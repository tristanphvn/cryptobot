package handlers

import (
	"net/http/httptest"
	"testing"
)

// CRY-75: the WebSocket upgrade only accepts the dashboard origins.
func TestUpgrader_CheckOrigin(t *testing.T) {
	up := newUpgrader([]string{"http://localhost:3002", "http://127.0.0.1:3002/"})
	for origin, want := range map[string]bool{
		"http://localhost:3002":  true,
		"HTTP://LOCALHOST:3002/": true,
		"http://127.0.0.1:3002":  true,
		"https://evil.example":   false,
		"http://localhost:4000":  false,
		"":                       true, // non-browser client sends no Origin
	} {
		req := httptest.NewRequest("GET", "/ws", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if got := up.CheckOrigin(req); got != want {
			t.Errorf("origin %q allowed=%v want %v", origin, got, want)
		}
	}
	req := httptest.NewRequest("GET", "/ws", nil)
	req.Header.Set("Origin", "https://evil.example")
	if newUpgrader(nil).CheckOrigin(req) {
		t.Error("an empty allow-list must reject browser origins")
	}
}
