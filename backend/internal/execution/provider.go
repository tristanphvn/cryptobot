// Package execution is the provider-agnostic seam between the trade
// executor and a venue (Binance for now). Standard, not a bandaid: the
// executor talks ONLY to ExecutionProvider. Adding a second exchange
// is a new implementation of this interface, not a refactor.
package execution

import (
	"context"
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// OrderSide / OrderType are venue-agnostic. The Binance impl maps them
// to its string literals on the wire.
type OrderSide string

const (
	SideBuy  OrderSide = "BUY"
	SideSell OrderSide = "SELL"
)

type OrderType string

const (
	OrderMarket OrderType = "MARKET"
)

// OrderRequest is what the executor hands to a provider.
//
// For MARKET BUY use QuoteQuantity (USDT amount) — risk manager
// computes "spend 2% of equity" naturally. For MARKET SELL use
// Quantity (base asset) — we're unwinding an existing position of
// known base size. The provider is responsible for LOT_SIZE rounding
// and MIN_NOTIONAL validation before signing.
type OrderRequest struct {
	Symbol        string
	ClientOrderID string // "cry5-<pending_signal_id>" — idempotency key
	Side          OrderSide
	Type          OrderType
	Quantity      decimal.Decimal // for SELL (and BUY when QuoteQuantity unset)
	QuoteQuantity decimal.Decimal // for BUY MARKET via quoteOrderQty (USDT)
	RecvWindowMs  int64           // optional override; default 5000
}

// OrderResponse is the terminal-status snapshot. PlaceOrder MUST block
// (with timeout) until Status.IsTerminal() or return ErrPollTimeout —
// callers never observe NEW / PARTIALLY_FILLED here.
type OrderResponse struct {
	ClientOrderID   string
	ExchangeOrderID int64
	Symbol          string
	Side            OrderSide
	Status          BinanceStatus
	FilledQty       decimal.Decimal // sum of fills.qty
	AvgFillPrice    decimal.Decimal // qty-weighted average
	CumQuoteQty     decimal.Decimal // cummulativeQuoteQty (USDT spent/received)
	Fills           []Fill
	TransactTime    time.Time
}

type Fill struct {
	Price           decimal.Decimal
	Qty             decimal.Decimal
	Commission      decimal.Decimal
	CommissionAsset string
}

// BinanceStatus mirrors the Binance order status enum. Kept in this
// package (not internal/exchange) because callers reason about
// terminal vs non-terminal, not Binance specifics.
type BinanceStatus string

const (
	StatusNew             BinanceStatus = "NEW"
	StatusPartiallyFilled BinanceStatus = "PARTIALLY_FILLED"
	StatusFilled          BinanceStatus = "FILLED"
	StatusCanceled        BinanceStatus = "CANCELED"
	StatusPendingCancel   BinanceStatus = "PENDING_CANCEL"
	StatusRejected        BinanceStatus = "REJECTED"
	StatusExpired         BinanceStatus = "EXPIRED"
)

// IsTerminal returns true when no further state change is possible
// without operator action. NEW and PARTIALLY_FILLED keep changing;
// everything else is final.
func (s BinanceStatus) IsTerminal() bool {
	switch s {
	case StatusFilled, StatusCanceled, StatusExpired, StatusRejected:
		return true
	}
	return false
}

// SymbolFilters are the only exchange-precision rules the executor
// needs. Loaded once from GET /api/v3/exchangeInfo, cached, refreshed
// daily. Standard, not a bandaid: no hardcoded precision anywhere.
type SymbolFilters struct {
	Symbol      string
	BaseAsset   string
	QuoteAsset  string
	StepSize    decimal.Decimal // LOT_SIZE.stepSize
	MinQty      decimal.Decimal // LOT_SIZE.minQty
	MaxQty      decimal.Decimal // LOT_SIZE.maxQty
	TickSize    decimal.Decimal // PRICE_FILTER.tickSize
	MinNotional decimal.Decimal // MIN_NOTIONAL or NOTIONAL
}

// AccountBalance is the per-asset result of GET /api/v3/account. Free
// is "available for trading" (excludes locked in open orders). The
// executor uses Free["USDT"] for position sizing.
type AccountBalance struct {
	Asset  string
	Free   decimal.Decimal
	Locked decimal.Decimal
}

// ExecutionProvider is the seam between executor and venue. All
// methods MUST be safe for concurrent use.
type ExecutionProvider interface {
	// PlaceOrder signs, submits, and polls until terminal status (or
	// PollTimeout). Provider does LOT_SIZE / MIN_NOTIONAL normalization
	// internally; callers pass raw qty/quoteQty.
	PlaceOrder(ctx context.Context, req OrderRequest) (OrderResponse, error)

	// GetOrder fetches current status by client_order_id. Used by the
	// reconciler. Returns ErrOrderNotFound when Binance reports -2013.
	GetOrder(ctx context.Context, symbol, clientOrderID string) (OrderResponse, error)

	// GetAccountBalance returns balances keyed by asset. Callers
	// cache; provider does not.
	GetAccountBalance(ctx context.Context) (map[string]AccountBalance, error)

	// GetSymbolFilters returns cached filters. May trigger a one-time
	// exchangeInfo fetch on cold lookup; never blocks on rate-limited
	// refresh.
	GetSymbolFilters(ctx context.Context, symbol string) (SymbolFilters, error)

	// GetTickerPrice returns the latest price of symbol from the public
	// ticker. Used to value commission paid in a third asset (BNB when
	// "Use BNB for fees" is on) in the quote asset. CRY-35.
	GetTickerPrice(ctx context.Context, symbol string) (decimal.Decimal, error)
}

// Sentinel errors. Distinct so the executor's error-categorisation
// matrix (CRY-15 plan §12) can route to MarkFailed vs leave-for-
// reconciliation without string matching.
var (
	ErrOrderNotFound       = errors.New("execution: order not found (binance -2013)")
	ErrSymbolNotFound      = errors.New("execution: symbol not in exchangeInfo")
	ErrLotSizeTooSmall     = errors.New("execution: qty below minQty after stepSize rounding")
	ErrNotionalTooSmall    = errors.New("execution: notional below minNotional")
	ErrInsufficientBalance = errors.New("execution: insufficient balance (binance -2010)")
	ErrSignatureRejected   = errors.New("execution: invalid signature (binance -1022)")
	ErrPollTimeout         = errors.New("execution: order did not reach terminal status before timeout")
	ErrRateLimited         = errors.New("execution: rate limited (429/418)")
	ErrAmbiguousPlacement  = errors.New("execution: placement outcome ambiguous (5xx or network) — leave for reconciler")
)
