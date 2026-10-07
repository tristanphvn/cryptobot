package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cryptobot/internal/execution"

	"github.com/shopspring/decimal"
)

// newFakeBinance spins up a httptest server that lets each test
// register handlers for the few Binance endpoints we hit. Keeping the
// fake explicit (instead of a "smart" mock) prevents the tests from
// drifting away from real Binance semantics — every handler is a real
// HTTP handler returning real bytes.
type fakeBinance struct {
	srv        *httptest.Server
	mu         sync.Mutex
	exchInfo   string
	orders     map[string]string // origClientOrderId → JSON body
	placeCount atomic.Int32
	postOrder  func(t *testing.T, params map[string]string) (status int, body string)
	getOrder   func(t *testing.T, params map[string]string) (status int, body string)
	tb         *testing.T
}

func newFakeBinance(t *testing.T) *fakeBinance {
	f := &fakeBinance{
		orders:   map[string]string{},
		tb:       t,
		exchInfo: defaultExchangeInfo(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/exchangeInfo", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(f.exchInfo))
	})
	mux.HandleFunc("/api/v3/order", func(w http.ResponseWriter, r *http.Request) {
		params := readParams(r)
		switch r.Method {
		case http.MethodPost:
			f.placeCount.Add(1)
			if f.postOrder == nil {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"code":-9999,"msg":"test did not set postOrder"}`))
				return
			}
			status, body := f.postOrder(f.tb, params)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		case http.MethodGet:
			if f.getOrder == nil {
				// Fall back to canned orders map.
				if body, ok := f.orders[params["origClientOrderId"]]; ok {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(body))
					return
				}
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":-2013,"msg":"Order does not exist."}`))
				return
			}
			status, body := f.getOrder(f.tb, params)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}
	})
	mux.HandleFunc("/api/v3/account", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"balances": [
				{"asset":"USDT","free":"500.00","locked":"0.00"},
				{"asset":"BTC","free":"0.01","locked":"0.00"}
			]
		}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func readParams(r *http.Request) map[string]string {
	_ = r.ParseForm()
	out := map[string]string{}
	for k, v := range r.Form {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

// defaultExchangeInfo returns a minimal exchangeInfo body covering
// BTCUSDT with realistic filters so normalize tests are meaningful.
func defaultExchangeInfo() string {
	return `{
		"symbols": [
			{
				"symbol": "BTCUSDT",
				"baseAsset": "BTC",
				"quoteAsset": "USDT",
				"filters": [
					{"filterType":"LOT_SIZE","stepSize":"0.00001","minQty":"0.00001","maxQty":"9000"},
					{"filterType":"PRICE_FILTER","tickSize":"0.01"},
					{"filterType":"NOTIONAL","minNotional":"10"}
				]
			}
		]
	}`
}

func newClientForFake(t *testing.T, f *fakeBinance) *BinanceOrderClient {
	c := NewBinanceOrderClient(f.srv.URL, "test-key", "test-secret", 5000)
	c.SetPollPolicy(5*time.Millisecond, 20*time.Millisecond, 2*time.Second)
	if err := c.refreshExchangeInfo(context.Background()); err != nil {
		t.Fatalf("prime exchangeInfo: %v", err)
	}
	return c
}

// ─────────────────────────────────────────────────────────────────────
// PlaceOrder
// ─────────────────────────────────────────────────────────────────────

func TestPlaceOrder_HappyPathFilled(t *testing.T) {
	f := newFakeBinance(t)
	f.postOrder = func(t *testing.T, p map[string]string) (int, string) {
		if p["newClientOrderId"] != "cry5-42" {
			t.Fatalf("ClientOrderID not passed through: %q", p["newClientOrderId"])
		}
		if p["side"] != "BUY" || p["type"] != "MARKET" {
			t.Fatalf("unexpected side/type: %v", p)
		}
		return 200, `{
			"symbol":"BTCUSDT","orderId":12345,"clientOrderId":"cry5-42",
			"transactTime":1700000000000,"status":"FILLED","side":"BUY",
			"executedQty":"0.00150000","cummulativeQuoteQty":"100.00000000",
			"fills":[{"price":"66666.66","qty":"0.00150000","commission":"0.00000150","commissionAsset":"BTC"}]
		}`
	}
	c := newClientForFake(t, f)

	resp, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol:        "BTCUSDT",
		ClientOrderID: "cry5-42",
		Side:          execution.SideBuy,
		Type:          execution.OrderMarket,
		QuoteQuantity: decimal.NewFromInt(100),
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if resp.Status != execution.StatusFilled {
		t.Fatalf("status=%s want FILLED", resp.Status)
	}
	if !resp.FilledQty.Equal(decimal.RequireFromString("0.00150000")) {
		t.Fatalf("filledQty=%s", resp.FilledQty)
	}
	if !resp.AvgFillPrice.Equal(decimal.RequireFromString("66666.66666666666666666667")) {
		// avg = cum_quote / filled = 100 / 0.0015 = 66666.6666...
		// shopspring/decimal default precision uses 16 dp on Div.
		// Equal compares value, so accept the rounded form below too.
		if !resp.AvgFillPrice.Round(2).Equal(decimal.RequireFromString("66666.67")) {
			t.Fatalf("avgFillPrice=%s", resp.AvgFillPrice)
		}
	}
	if resp.ExchangeOrderID != 12345 {
		t.Fatalf("exchangeOrderID=%d", resp.ExchangeOrderID)
	}
}

func TestPlaceOrder_PartialThenFilled(t *testing.T) {
	f := newFakeBinance(t)
	// First call: POST returns NEW (non-terminal).
	f.postOrder = func(t *testing.T, p map[string]string) (int, string) {
		return 200, `{
			"symbol":"BTCUSDT","orderId":1,"clientOrderId":"cry5-7",
			"transactTime":1700000000000,"status":"NEW","side":"BUY",
			"executedQty":"0","cummulativeQuoteQty":"0","fills":[]
		}`
	}
	calls := 0
	f.getOrder = func(t *testing.T, p map[string]string) (int, string) {
		calls++
		switch calls {
		case 1:
			return 200, `{"symbol":"BTCUSDT","orderId":1,"clientOrderId":"cry5-7","status":"PARTIALLY_FILLED","side":"BUY","executedQty":"0.0005","cummulativeQuoteQty":"33","fills":[]}`
		default:
			return 200, `{"symbol":"BTCUSDT","orderId":1,"clientOrderId":"cry5-7","status":"FILLED","side":"BUY","executedQty":"0.0015","cummulativeQuoteQty":"100","fills":[]}`
		}
	}
	c := newClientForFake(t, f)
	resp, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", ClientOrderID: "cry5-7", Side: execution.SideBuy,
		Type: execution.OrderMarket, QuoteQuantity: decimal.NewFromInt(100),
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if resp.Status != execution.StatusFilled {
		t.Fatalf("status=%s want FILLED", resp.Status)
	}
	if calls < 2 {
		t.Fatalf("expected at least 2 GET /order polls, got %d", calls)
	}
}

func TestPlaceOrder_Idempotent_SameClientOrderID(t *testing.T) {
	f := newFakeBinance(t)
	// Real Binance dedupes by newClientOrderId; the fake mimics by
	// returning the same orderId regardless of how many POSTs arrive.
	f.postOrder = func(t *testing.T, p map[string]string) (int, string) {
		return 200, fmt.Sprintf(`{
			"symbol":"BTCUSDT","orderId":999,"clientOrderId":%q,
			"status":"FILLED","side":"BUY",
			"executedQty":"0.0015","cummulativeQuoteQty":"100","fills":[]
		}`, p["newClientOrderId"])
	}
	c := newClientForFake(t, f)
	for i := 0; i < 3; i++ {
		resp, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
			Symbol: "BTCUSDT", ClientOrderID: "cry5-99", Side: execution.SideBuy,
			Type: execution.OrderMarket, QuoteQuantity: decimal.NewFromInt(100),
		})
		if err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if resp.ExchangeOrderID != 999 {
			t.Fatalf("retry %d returned different orderId=%d", i, resp.ExchangeOrderID)
		}
	}
	if got := f.placeCount.Load(); got != 3 {
		t.Fatalf("expected 3 POSTs (caller retried), got %d", got)
	}
}

func TestPlaceOrder_InsufficientBalance_Maps_2010(t *testing.T) {
	f := newFakeBinance(t)
	f.postOrder = func(t *testing.T, p map[string]string) (int, string) {
		return http.StatusBadRequest, `{"code":-2010,"msg":"Account has insufficient balance for requested action."}`
	}
	c := newClientForFake(t, f)
	_, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", ClientOrderID: "cry5-1", Side: execution.SideBuy,
		Type: execution.OrderMarket, QuoteQuantity: decimal.NewFromInt(100),
	})
	if !errors.Is(err, execution.ErrInsufficientBalance) {
		t.Fatalf("err=%v want ErrInsufficientBalance", err)
	}
}

func TestPlaceOrder_SignatureRejected_Maps_1022(t *testing.T) {
	f := newFakeBinance(t)
	f.postOrder = func(t *testing.T, p map[string]string) (int, string) {
		return http.StatusBadRequest, `{"code":-1022,"msg":"Signature for this request is not valid."}`
	}
	c := newClientForFake(t, f)
	_, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", ClientOrderID: "cry5-1", Side: execution.SideBuy,
		Type: execution.OrderMarket, QuoteQuantity: decimal.NewFromInt(100),
	})
	if !errors.Is(err, execution.ErrSignatureRejected) {
		t.Fatalf("err=%v want ErrSignatureRejected", err)
	}
}

func TestPlaceOrder_RateLimited_Maps_429(t *testing.T) {
	f := newFakeBinance(t)
	f.postOrder = func(t *testing.T, p map[string]string) (int, string) {
		return http.StatusTooManyRequests, `{"code":-1003,"msg":"Too many requests."}`
	}
	c := newClientForFake(t, f)
	_, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", ClientOrderID: "cry5-1", Side: execution.SideBuy,
		Type: execution.OrderMarket, QuoteQuantity: decimal.NewFromInt(100),
	})
	if !errors.Is(err, execution.ErrRateLimited) {
		t.Fatalf("err=%v want ErrRateLimited", err)
	}
}

func TestPlaceOrder_5xx_Maps_AmbiguousPlacement(t *testing.T) {
	f := newFakeBinance(t)
	f.postOrder = func(t *testing.T, p map[string]string) (int, string) {
		return http.StatusBadGateway, `upstream broken`
	}
	c := newClientForFake(t, f)
	_, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", ClientOrderID: "cry5-1", Side: execution.SideBuy,
		Type: execution.OrderMarket, QuoteQuantity: decimal.NewFromInt(100),
	})
	if !errors.Is(err, execution.ErrAmbiguousPlacement) {
		t.Fatalf("err=%v want ErrAmbiguousPlacement", err)
	}
}

func TestPlaceOrder_PollTimeout(t *testing.T) {
	f := newFakeBinance(t)
	f.postOrder = func(t *testing.T, p map[string]string) (int, string) {
		return 200, `{"symbol":"BTCUSDT","orderId":1,"clientOrderId":"cry5-1","status":"NEW","side":"BUY","executedQty":"0","cummulativeQuoteQty":"0","fills":[]}`
	}
	f.getOrder = func(t *testing.T, p map[string]string) (int, string) {
		return 200, `{"symbol":"BTCUSDT","orderId":1,"clientOrderId":"cry5-1","status":"PARTIALLY_FILLED","side":"BUY","executedQty":"0.0001","cummulativeQuoteQty":"6","fills":[]}`
	}
	c := NewBinanceOrderClient(f.srv.URL, "k", "s", 5000)
	c.SetPollPolicy(5*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond)
	if err := c.refreshExchangeInfo(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	_, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", ClientOrderID: "cry5-1", Side: execution.SideBuy,
		Type: execution.OrderMarket, QuoteQuantity: decimal.NewFromInt(100),
	})
	if !errors.Is(err, execution.ErrPollTimeout) {
		t.Fatalf("err=%v want ErrPollTimeout", err)
	}
}

func TestPlaceOrder_BelowMinNotional(t *testing.T) {
	f := newFakeBinance(t)
	c := newClientForFake(t, f)
	_, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", ClientOrderID: "cry5-1", Side: execution.SideBuy,
		Type: execution.OrderMarket, QuoteQuantity: decimal.NewFromInt(5), // below 10
	})
	if !errors.Is(err, execution.ErrNotionalTooSmall) {
		t.Fatalf("err=%v want ErrNotionalTooSmall", err)
	}
	if got := f.placeCount.Load(); got != 0 {
		t.Fatalf("normalize must reject before any POST; got %d POSTs", got)
	}
}

func TestPlaceOrder_SellNormalizesLotSize(t *testing.T) {
	f := newFakeBinance(t)
	gotQty := ""
	f.postOrder = func(t *testing.T, p map[string]string) (int, string) {
		gotQty = p["quantity"]
		return 200, `{"symbol":"BTCUSDT","orderId":2,"clientOrderId":"cry5-1","status":"FILLED","side":"SELL","executedQty":"0.00150","cummulativeQuoteQty":"100","fills":[]}`
	}
	c := newClientForFake(t, f)
	// stepSize=0.00001 → 0.001234 floors to 0.00123.
	_, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", ClientOrderID: "cry5-1", Side: execution.SideSell,
		Type: execution.OrderMarket, Quantity: decimal.RequireFromString("0.001234"),
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if gotQty != "0.00123" {
		t.Fatalf("qty sent=%q want 0.00123 (floored to stepSize=0.00001)", gotQty)
	}
}

func TestPlaceOrder_BelowMinQty(t *testing.T) {
	f := newFakeBinance(t)
	c := newClientForFake(t, f)
	_, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", ClientOrderID: "cry5-1", Side: execution.SideSell,
		Type: execution.OrderMarket, Quantity: decimal.RequireFromString("0.0000001"),
	})
	if !errors.Is(err, execution.ErrLotSizeTooSmall) {
		t.Fatalf("err=%v want ErrLotSizeTooSmall", err)
	}
}

func TestPlaceOrder_RequiresClientOrderID(t *testing.T) {
	f := newFakeBinance(t)
	c := newClientForFake(t, f)
	_, err := c.PlaceOrder(context.Background(), execution.OrderRequest{
		Symbol: "BTCUSDT", Side: execution.SideBuy, Type: execution.OrderMarket,
		QuoteQuantity: decimal.NewFromInt(100),
	})
	if err == nil || !strings.Contains(err.Error(), "ClientOrderID required") {
		t.Fatalf("err=%v want ClientOrderID required", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// GetOrder
// ─────────────────────────────────────────────────────────────────────

func TestGetOrder_NotFound_Maps_2013(t *testing.T) {
	f := newFakeBinance(t)
	c := newClientForFake(t, f)
	_, err := c.GetOrder(context.Background(), "BTCUSDT", "cry5-does-not-exist")
	if !errors.Is(err, execution.ErrOrderNotFound) {
		t.Fatalf("err=%v want ErrOrderNotFound", err)
	}
}

func TestGetOrder_Filled(t *testing.T) {
	f := newFakeBinance(t)
	f.orders["cry5-5"] = `{
		"symbol":"BTCUSDT","orderId":777,"clientOrderId":"cry5-5",
		"status":"FILLED","side":"BUY",
		"executedQty":"0.0015","cummulativeQuoteQty":"100","fills":[]
	}`
	c := newClientForFake(t, f)
	resp, err := c.GetOrder(context.Background(), "BTCUSDT", "cry5-5")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if resp.Status != execution.StatusFilled || resp.ExchangeOrderID != 777 {
		t.Fatalf("resp=%+v", resp)
	}
}

// ─────────────────────────────────────────────────────────────────────
// exchangeInfo + GetSymbolFilters
// ─────────────────────────────────────────────────────────────────────

func TestGetSymbolFilters_LoadsBTCUSDT(t *testing.T) {
	f := newFakeBinance(t)
	c := newClientForFake(t, f)
	got, err := c.GetSymbolFilters(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("GetSymbolFilters: %v", err)
	}
	if !got.StepSize.Equal(decimal.RequireFromString("0.00001")) {
		t.Fatalf("stepSize=%s", got.StepSize)
	}
	if !got.MinNotional.Equal(decimal.RequireFromString("10")) {
		t.Fatalf("minNotional=%s", got.MinNotional)
	}
	if got.BaseAsset != "BTC" || got.QuoteAsset != "USDT" {
		t.Fatalf("assets=%s/%s", got.BaseAsset, got.QuoteAsset)
	}
}

func TestGetSymbolFilters_SymbolNotFound(t *testing.T) {
	f := newFakeBinance(t)
	c := newClientForFake(t, f)
	_, err := c.GetSymbolFilters(context.Background(), "MISSINGUSDT")
	if !errors.Is(err, execution.ErrSymbolNotFound) {
		t.Fatalf("err=%v want ErrSymbolNotFound", err)
	}
}

func TestRefreshLoop_ReloadsOnTick(t *testing.T) {
	f := newFakeBinance(t)
	c := newClientForFake(t, f)
	c.SetRefreshInterval(20 * time.Millisecond)

	// Swap the exchangeInfo body so the next refresh observes a change.
	f.mu.Lock()
	f.exchInfo = strings.Replace(f.exchInfo, `"minNotional":"10"`, `"minNotional":"25"`, 1)
	f.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.refreshLoop(ctx)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		got, _ := c.GetSymbolFilters(context.Background(), "BTCUSDT")
		if got.MinNotional.Equal(decimal.RequireFromString("25")) {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("refresh loop did not pick up new minNotional")
}

// ─────────────────────────────────────────────────────────────────────
// GetAccountBalance
// ─────────────────────────────────────────────────────────────────────

func TestGetAccountBalance(t *testing.T) {
	f := newFakeBinance(t)
	c := newClientForFake(t, f)
	bals, err := c.GetAccountBalance(context.Background())
	if err != nil {
		t.Fatalf("GetAccountBalance: %v", err)
	}
	usdt, ok := bals["USDT"]
	if !ok {
		t.Fatal("USDT balance missing")
	}
	if !usdt.Free.Equal(decimal.RequireFromString("500.00")) {
		t.Fatalf("USDT free=%s want 500.00", usdt.Free)
	}
}

// Compile-time guard: anchors fmt import (used in idempotency test) so
// future refactors don't drop it accidentally.
var _ = json.Unmarshal
