package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"cryptobot/internal/api/handlers"
	"cryptobot/internal/riskmanager"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func newTestRouter(t *testing.T, token string) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	risk := riskmanager.New(db, riskmanager.DefaultConfig())
	return NewRouter(db, handlers.NewHub(nil), risk, nil, nil, nil, nil, nil, nil, token, nil,
		[]string{"http://localhost:3002"})
}

func do(h http.Handler, method, path, bearer string) int {
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

// CRY-75: with an operator token configured, re-arming the circuit
// breaker and launching a backtest require it; reads stay public.
func TestRouter_OperatorActionsRequireToken(t *testing.T) {
	r := newTestRouter(t, "s3cret")

	for _, path := range []string{"/api/v1/risk/reset", "/api/v1/backtest"} {
		if code := do(r, http.MethodPost, path, ""); code != http.StatusUnauthorized {
			t.Errorf("POST %s without token = %d, want 401", path, code)
		}
		if code := do(r, http.MethodPost, path, "wrong"); code != http.StatusUnauthorized {
			t.Errorf("POST %s with wrong token = %d, want 401", path, code)
		}
	}
	if code := do(r, http.MethodPost, "/api/v1/risk/reset", "s3cret"); code != http.StatusOK {
		t.Errorf("POST /risk/reset with token = %d, want 200", code)
	}
	if code := do(r, http.MethodGet, "/api/v1/risk/status", ""); code != http.StatusOK {
		t.Errorf("GET /risk/status = %d, want 200 (reads stay public)", code)
	}
}

func TestRouter_NoTokenConfiguredKeepsLocalDevOpen(t *testing.T) {
	r := newTestRouter(t, "")
	if code := do(r, http.MethodPost, "/api/v1/risk/reset", ""); code != http.StatusOK {
		t.Errorf("POST /risk/reset with no token configured = %d, want 200", code)
	}
}

func TestRouter_CORSOnlyAllowsConfiguredOrigins(t *testing.T) {
	r := newTestRouter(t, "")
	for origin, want := range map[string]bool{"http://localhost:3002": true, "https://evil.example": false} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/risk/status", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if got := w.Header().Get("Access-Control-Allow-Origin") == origin; got != want {
			t.Errorf("origin %s allowed=%v want %v", origin, got, want)
		}
	}
}
