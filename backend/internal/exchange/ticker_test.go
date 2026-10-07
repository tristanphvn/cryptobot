package exchange

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetTickerPrice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v3/ticker/price" || r.URL.Query().Get("symbol") != "BNBUSDT" {
			http.Error(w, `{"code":-1121,"msg":"Invalid symbol."}`, http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-MBX-APIKEY") != "" {
			t.Error("public ticker must not send the API key")
		}
		w.Write([]byte(`{"symbol":"BNBUSDT","price":"601.25000000"}`))
	}))
	defer srv.Close()
	c := &BinanceOrderClient{restURL: srv.URL, apiKey: "k", http: srv.Client()}

	p, err := c.GetTickerPrice(context.Background(), "BNBUSDT")
	if err != nil || p.String() != "601.25" {
		t.Fatalf("price=%s err=%v want 601.25", p, err)
	}
	if _, err := c.GetTickerPrice(context.Background(), "NOPEUSDT"); err == nil {
		t.Fatal("non-200 must return an error")
	}
}
