package exchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cryptobot/internal/execution"

	"github.com/shopspring/decimal"
)

// BinanceOrderClient is the production ExecutionProvider talking to a
// Binance spot REST endpoint. Standard, not a bandaid: every wire-level
// concern (signing, recv-window, LOT_SIZE / MIN_NOTIONAL rounding,
// terminal-status polling, error-code mapping) sits behind this single
// type so the executor stays venue-agnostic.
//
// Safe for concurrent use. The exchangeInfo cache is RW-locked and
// refreshed on a background ticker (started by Start); cold lookups
// trigger a one-shot fetch.
type BinanceOrderClient struct {
	restURL    string
	apiKey     string
	apiSecret  string
	recvWindow int64
	http       *http.Client

	// poll cadence + cap. PlaceOrder loops until terminal status or
	// pollTimeout. Defaults: 200ms initial, 2s max, 30s overall.
	pollInitial time.Duration
	pollMax     time.Duration
	pollTimeout time.Duration

	// exchangeInfo cache. Filters are looked up by symbol; missing →
	// one-shot fetch. refreshEvery (default 24h) drives the bg ticker
	// started by Start.
	mu           sync.RWMutex
	filters      map[string]execution.SymbolFilters
	loadedAt     time.Time
	refreshEvery time.Duration
}

// NewBinanceOrderClient builds the client with production defaults. Pass
// recvWindow=0 for the 5000ms Binance default.
func NewBinanceOrderClient(restURL, apiKey, apiSecret string, recvWindow int64) *BinanceOrderClient {
	if recvWindow <= 0 {
		recvWindow = 5000
	}
	return &BinanceOrderClient{
		restURL:    strings.TrimRight(restURL, "/"),
		apiKey:     apiKey,
		apiSecret:  apiSecret,
		recvWindow: recvWindow,
		http: &http.Client{
			Timeout: 15 * time.Second,
		},
		pollInitial:  200 * time.Millisecond,
		pollMax:      2 * time.Second,
		pollTimeout:  30 * time.Second,
		filters:      make(map[string]execution.SymbolFilters),
		refreshEvery: 24 * time.Hour,
	}
}

// SetPollPolicy lets tests shrink the polling window. Production should
// not call this — the defaults are tuned for real Binance latencies.
func (c *BinanceOrderClient) SetPollPolicy(initial, max, timeout time.Duration) {
	c.pollInitial = initial
	c.pollMax = max
	c.pollTimeout = timeout
}

// SetRefreshInterval overrides the daily exchangeInfo refresh cadence
// (tests use a short interval to exercise the ticker).
func (c *BinanceOrderClient) SetRefreshInterval(d time.Duration) {
	c.refreshEvery = d
}

// Start prefetches exchangeInfo and launches a background ticker that
// refreshes it every refreshEvery. Returns the prefetch error so a
// startup failure is surfaced loudly. Cancels via ctx.
func (c *BinanceOrderClient) Start(ctx context.Context) error {
	if err := c.refreshExchangeInfo(ctx); err != nil {
		return fmt.Errorf("prefetch exchangeInfo: %w", err)
	}
	go c.refreshLoop(ctx)
	return nil
}

func (c *BinanceOrderClient) refreshLoop(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[binance-order] exchangeInfo refresh panic: %v", r)
		}
	}()
	t := time.NewTicker(c.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.refreshExchangeInfo(ctx); err != nil {
				log.Printf("[binance-order] exchangeInfo refresh err: %v", err)
			}
		}
	}
}

// ─────────────────────────────────────────────────────────────────────
// ExecutionProvider impl
// ─────────────────────────────────────────────────────────────────────

// PlaceOrder signs + submits + polls until terminal. See spec on
// execution.ExecutionProvider. Normalises qty against the cached
// LOT_SIZE / MIN_NOTIONAL filters before signing.
func (c *BinanceOrderClient) PlaceOrder(ctx context.Context, req execution.OrderRequest) (execution.OrderResponse, error) {
	if req.ClientOrderID == "" {
		return execution.OrderResponse{}, fmt.Errorf("execution: ClientOrderID required for idempotency")
	}

	filters, err := c.GetSymbolFilters(ctx, req.Symbol)
	if err != nil {
		return execution.OrderResponse{}, err
	}

	params := url.Values{}
	params.Set("symbol", req.Symbol)
	params.Set("side", string(req.Side))
	params.Set("type", string(req.Type))
	params.Set("newClientOrderId", req.ClientOrderID)
	params.Set("newOrderRespType", "FULL")

	switch {
	case req.Side == execution.SideBuy && req.QuoteQuantity.GreaterThan(decimal.Zero):
		// MARKET BUY by USDT amount. MIN_NOTIONAL applies directly.
		if req.QuoteQuantity.LessThan(filters.MinNotional) {
			return execution.OrderResponse{}, fmt.Errorf("%w: quoteOrderQty=%s minNotional=%s",
				execution.ErrNotionalTooSmall, req.QuoteQuantity, filters.MinNotional)
		}
		params.Set("quoteOrderQty", req.QuoteQuantity.String())
	default:
		// MARKET SELL (and BUY-by-base) by base qty — must round to LOT_SIZE
		// stepSize and clear MIN_NOTIONAL given a best-effort reference price.
		qty, err := normalizeQty(req.Quantity, filters)
		if err != nil {
			return execution.OrderResponse{}, err
		}
		params.Set("quantity", qty.String())
	}

	recv := req.RecvWindowMs
	if recv <= 0 {
		recv = c.recvWindow
	}
	signed := SignQuery(params, c.apiSecret, recv)

	resp, err := c.doSigned(ctx, http.MethodPost, "/api/v3/order", signed)
	if err != nil {
		return execution.OrderResponse{}, err
	}
	first, err := parseOrderResponse(resp, req)
	if err != nil {
		return execution.OrderResponse{}, err
	}
	if first.Status.IsTerminal() {
		return first, nil
	}
	return c.pollUntilTerminal(ctx, req)
}

// GetOrder looks up an order by origClientOrderId. -2013 → ErrOrderNotFound
// so the reconciler can disambiguate "never reached Binance" from "Binance
// has it in a terminal state".
func (c *BinanceOrderClient) GetOrder(ctx context.Context, symbol, clientOrderID string) (execution.OrderResponse, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("origClientOrderId", clientOrderID)
	signed := SignQuery(params, c.apiSecret, c.recvWindow)

	body, err := c.doSigned(ctx, http.MethodGet, "/api/v3/order", signed)
	if err != nil {
		return execution.OrderResponse{}, err
	}
	return parseOrderResponse(body, execution.OrderRequest{
		Symbol:        symbol,
		ClientOrderID: clientOrderID,
	})
}

// GetAccountBalance returns spot balances keyed by asset.
func (c *BinanceOrderClient) GetAccountBalance(ctx context.Context) (map[string]execution.AccountBalance, error) {
	signed := SignQuery(url.Values{}, c.apiSecret, c.recvWindow)
	body, err := c.doSigned(ctx, http.MethodGet, "/api/v3/account", signed)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Balances []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode account: %w", err)
	}
	out := make(map[string]execution.AccountBalance, len(raw.Balances))
	for _, b := range raw.Balances {
		out[b.Asset] = execution.AccountBalance{
			Asset:  b.Asset,
			Free:   parseDecimalOrZero(b.Free),
			Locked: parseDecimalOrZero(b.Locked),
		}
	}
	return out, nil
}

// GetTickerPrice returns the latest trade price from the public
// /api/v3/ticker/price endpoint (unsigned). CRY-35: values BNB-paid
// commission in USDT at settle time.
func (c *BinanceOrderClient) GetTickerPrice(ctx context.Context, symbol string) (decimal.Decimal, error) {
	u := c.restURL + "/api/v3/ticker/price?symbol=" + url.QueryEscape(symbol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return decimal.Zero, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return decimal.Zero, fmt.Errorf("ticker %s: %w", symbol, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return decimal.Zero, fmt.Errorf("ticker %s %d: %s", symbol, resp.StatusCode, snippet(body))
	}
	var raw struct {
		Price string `json:"price"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return decimal.Zero, fmt.Errorf("decode ticker %s: %w", symbol, err)
	}
	price := parseDecimalOrZero(raw.Price)
	if !price.IsPositive() {
		return decimal.Zero, fmt.Errorf("ticker %s: non-positive price %q", symbol, raw.Price)
	}
	return price, nil
}

// GetSymbolFilters returns cached filters, fetching exchangeInfo on cold
// miss. The fetch never blocks on the daily ticker — a miss is a one-shot
// refresh.
func (c *BinanceOrderClient) GetSymbolFilters(ctx context.Context, symbol string) (execution.SymbolFilters, error) {
	c.mu.RLock()
	f, ok := c.filters[symbol]
	c.mu.RUnlock()
	if ok {
		return f, nil
	}
	if err := c.refreshExchangeInfo(ctx); err != nil {
		return execution.SymbolFilters{}, err
	}
	c.mu.RLock()
	f, ok = c.filters[symbol]
	c.mu.RUnlock()
	if !ok {
		return execution.SymbolFilters{}, fmt.Errorf("%w: %s", execution.ErrSymbolNotFound, symbol)
	}
	return f, nil
}

// ─────────────────────────────────────────────────────────────────────
// internals
// ─────────────────────────────────────────────────────────────────────

// pollUntilTerminal repeatedly calls GET /api/v3/order with exp backoff
// until the response.Status.IsTerminal() or pollTimeout elapses.
func (c *BinanceOrderClient) pollUntilTerminal(parent context.Context, req execution.OrderRequest) (execution.OrderResponse, error) {
	ctx, cancel := context.WithTimeout(parent, c.pollTimeout)
	defer cancel()

	delay := c.pollInitial
	for {
		select {
		case <-ctx.Done():
			return execution.OrderResponse{}, execution.ErrPollTimeout
		case <-time.After(delay):
		}
		got, err := c.GetOrder(ctx, req.Symbol, req.ClientOrderID)
		if err != nil {
			// Transient errors (rate-limit, 5xx) — keep polling within
			// the timeout. ErrOrderNotFound during a placement-poll means
			// Binance lost the order; surface immediately so the caller
			// can route to reconciliation.
			if errors.Is(err, execution.ErrOrderNotFound) {
				return execution.OrderResponse{}, err
			}
			if errors.Is(err, execution.ErrRateLimited) || errors.Is(err, execution.ErrAmbiguousPlacement) {
				delay = nextDelay(delay, c.pollMax)
				continue
			}
			return execution.OrderResponse{}, err
		}
		if got.Status.IsTerminal() {
			return got, nil
		}
		delay = nextDelay(delay, c.pollMax)
	}
}

func nextDelay(cur, max time.Duration) time.Duration {
	d := cur * 2
	if d > max {
		return max
	}
	return d
}

// redactURLError strips the query string from a transport error. A GET
// / DELETE carries the signed payload (timestamp + signature) in the
// URL, and *url.Error prints the full URL — which would otherwise reach
// logs, the WS exit_failed alert and the positions API.
func redactURLError(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	redacted := *ue
	if u, perr := url.Parse(ue.URL); perr == nil {
		u.RawQuery = ""
		redacted.URL = u.String()
	} else {
		redacted.URL = "(redacted)"
	}
	return &redacted
}

// doSigned signs & sends a request and returns the body bytes. Maps
// Binance error codes to execution sentinel errors so the executor's
// error-categorisation matrix never depends on string matching.
func (c *BinanceOrderClient) doSigned(ctx context.Context, method, path, signed string) ([]byte, error) {
	full := c.restURL + path
	var req *http.Request
	var err error
	switch method {
	case http.MethodGet, http.MethodDelete:
		req, err = http.NewRequestWithContext(ctx, method, full+"?"+signed, nil)
	default:
		req, err = http.NewRequestWithContext(ctx, method, full, strings.NewReader(signed))
		if req != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		// Network-level failure is the canonical "ambiguous placement"
		// case — Binance MAY have received the order. The reconciler
		// later resolves via GET /api/v3/order by origClientOrderId.
		return nil, fmt.Errorf("%w: %v", execution.ErrAmbiguousPlacement, redactURLError(err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	switch {
	case resp.StatusCode == http.StatusOK:
		return body, nil
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusTeapot /* 418 — IP banned */ :
		return nil, fmt.Errorf("%w: %s", execution.ErrRateLimited, snippet(body))
	case resp.StatusCode >= 500:
		return nil, fmt.Errorf("%w: %d %s", execution.ErrAmbiguousPlacement, resp.StatusCode, snippet(body))
	}
	return nil, mapBinanceError(resp.StatusCode, body)
}

// binanceErr is the canonical 4xx body shape: {"code": -2013, "msg": "..."}
type binanceErr struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
}

func mapBinanceError(httpStatus int, body []byte) error {
	var e binanceErr
	if err := json.Unmarshal(body, &e); err != nil || e.Code == 0 {
		return fmt.Errorf("binance %d: %s", httpStatus, snippet(body))
	}
	switch e.Code {
	case -2013:
		return fmt.Errorf("%w: %s", execution.ErrOrderNotFound, e.Msg)
	case -2010:
		return fmt.Errorf("%w: %s", execution.ErrInsufficientBalance, e.Msg)
	case -1022, -1102:
		return fmt.Errorf("%w: %s", execution.ErrSignatureRejected, e.Msg)
	case -1003:
		return fmt.Errorf("%w: %s", execution.ErrRateLimited, e.Msg)
	}
	return fmt.Errorf("binance code=%d msg=%s", e.Code, e.Msg)
}

func snippet(b []byte) string {
	if len(b) > 200 {
		return string(b[:200]) + "..."
	}
	return string(b)
}

func parseDecimalOrZero(s string) decimal.Decimal {
	if s == "" {
		return decimal.Zero
	}
	d, _ := decimal.NewFromString(s)
	return d
}

// orderResp mirrors the FULL response shape (also used by GET /order).
type orderResp struct {
	Symbol              string `json:"symbol"`
	OrderID             int64  `json:"orderId"`
	ClientOrderID       string `json:"clientOrderId"`
	OrigClientOrderID   string `json:"origClientOrderId"`
	TransactTime        int64  `json:"transactTime"`
	UpdateTime          int64  `json:"updateTime"`
	Status              string `json:"status"`
	Side                string `json:"side"`
	ExecutedQty         string `json:"executedQty"`
	CumulativeQuoteQty  string `json:"cummulativeQuoteQty"` // sic — Binance ships the typo
	Fills               []struct {
		Price           string `json:"price"`
		Qty             string `json:"qty"`
		Commission      string `json:"commission"`
		CommissionAsset string `json:"commissionAsset"`
	} `json:"fills"`
}

func parseOrderResponse(body []byte, req execution.OrderRequest) (execution.OrderResponse, error) {
	var r orderResp
	if err := json.Unmarshal(body, &r); err != nil {
		return execution.OrderResponse{}, fmt.Errorf("decode order: %w", err)
	}
	clientID := r.ClientOrderID
	if clientID == "" {
		clientID = r.OrigClientOrderID
	}
	if clientID == "" {
		clientID = req.ClientOrderID
	}

	filled := parseDecimalOrZero(r.ExecutedQty)
	cumQuote := parseDecimalOrZero(r.CumulativeQuoteQty)
	var avg decimal.Decimal
	if filled.GreaterThan(decimal.Zero) {
		avg = cumQuote.Div(filled)
	}

	fills := make([]execution.Fill, 0, len(r.Fills))
	for _, f := range r.Fills {
		fills = append(fills, execution.Fill{
			Price:           parseDecimalOrZero(f.Price),
			Qty:             parseDecimalOrZero(f.Qty),
			Commission:      parseDecimalOrZero(f.Commission),
			CommissionAsset: f.CommissionAsset,
		})
	}

	transact := r.TransactTime
	if transact == 0 {
		transact = r.UpdateTime
	}

	return execution.OrderResponse{
		ClientOrderID:   clientID,
		ExchangeOrderID: r.OrderID,
		Symbol:          r.Symbol,
		Side:            execution.OrderSide(r.Side),
		Status:          execution.BinanceStatus(r.Status),
		FilledQty:       filled,
		AvgFillPrice:    avg,
		CumQuoteQty:     cumQuote,
		Fills:           fills,
		TransactTime:    time.UnixMilli(transact),
	}, nil
}

// ─────────────────────────────────────────────────────────────────────
// exchangeInfo
// ─────────────────────────────────────────────────────────────────────

func (c *BinanceOrderClient) refreshExchangeInfo(ctx context.Context) error {
	url := c.restURL + "/api/v3/exchangeInfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("exchangeInfo: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("exchangeInfo %d: %s", resp.StatusCode, snippet(body))
	}
	var raw struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
			Filters    []map[string]any `json:"filters"`
		} `json:"symbols"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("decode exchangeInfo: %w", err)
	}
	next := make(map[string]execution.SymbolFilters, len(raw.Symbols))
	for _, s := range raw.Symbols {
		f := execution.SymbolFilters{
			Symbol:     s.Symbol,
			BaseAsset:  s.BaseAsset,
			QuoteAsset: s.QuoteAsset,
		}
		for _, ft := range s.Filters {
			switch ft["filterType"] {
			case "LOT_SIZE":
				f.StepSize = pickDecimal(ft, "stepSize")
				f.MinQty = pickDecimal(ft, "minQty")
				f.MaxQty = pickDecimal(ft, "maxQty")
			case "PRICE_FILTER":
				f.TickSize = pickDecimal(ft, "tickSize")
			case "MIN_NOTIONAL", "NOTIONAL":
				if v := pickDecimal(ft, "minNotional"); !v.IsZero() {
					f.MinNotional = v
				}
			}
		}
		next[s.Symbol] = f
	}
	c.mu.Lock()
	c.filters = next
	c.loadedAt = time.Now()
	c.mu.Unlock()
	log.Printf("[binance-order] exchangeInfo loaded: %d symbols", len(next))
	return nil
}

func pickDecimal(m map[string]any, key string) decimal.Decimal {
	v, ok := m[key]
	if !ok {
		return decimal.Zero
	}
	switch t := v.(type) {
	case string:
		return parseDecimalOrZero(t)
	case float64:
		return decimal.NewFromFloat(t)
	}
	return decimal.Zero
}

// normalizeQty rounds qty DOWN to stepSize and validates against minQty.
// Standard, not a bandaid: rounding direction matters — rounding up could
// exceed available balance and bounce with -2010, while down silently
// truncates a few satoshi at worst. minNotional cannot be enforced here
// because the qty path lacks a reference price; the caller passes a price
// to MIN_NOTIONAL-check or uses quoteOrderQty (which Binance validates).
func normalizeQty(qty decimal.Decimal, f execution.SymbolFilters) (decimal.Decimal, error) {
	if f.StepSize.GreaterThan(decimal.Zero) {
		// floor(qty / step) * step
		steps := qty.Div(f.StepSize).Floor()
		qty = steps.Mul(f.StepSize)
	}
	if qty.LessThan(f.MinQty) {
		return decimal.Zero, fmt.Errorf("%w: qty=%s minQty=%s", execution.ErrLotSizeTooSmall, qty, f.MinQty)
	}
	return qty, nil
}

var _ execution.ExecutionProvider = (*BinanceOrderClient)(nil)
