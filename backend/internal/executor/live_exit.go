package executor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"cryptobot/internal/database/models"
	"cryptobot/internal/execution"

	"github.com/shopspring/decimal"
)

// CRY-30: in live mode a monitor exit (STOP_LOSS / TRAILING_STOP /
// TAKE_PROFIT) must sell on the venue before the position is closed in
// the DB. exitTracker keeps the per-symbol state that makes that safe
// to retry from a 5s monitor loop:
//
//   - inflight: at most one SELL per symbol at a time, shared with the
//     approval SELL path (submitClose), so a monitor tick and an
//     approved AI SELL can never both sell the same inventory.
//   - pendingID: a SELL whose outcome was ambiguous (network / poll
//     timeout). The next attempt looks it up by clientOrderId before
//     placing anything new, so an order that did reach the venue is
//     settled instead of doubled.
//   - attempt / nextAt: exponential backoff after a failed SELL so a
//     venue that keeps rejecting doesn't get hammered every tick (and
//     the audit trail doesn't grow a row per tick).
//
// State is in-memory: the bot runs as a single process. A restart
// loses the backoff (harmless — it retries) and any pendingID; the
// position stays OPEN in the DB either way, never closed without a fill.
type exitTracker struct {
	mu       sync.Mutex
	inflight map[string]bool
	state    map[string]*exitState
}

type exitState struct {
	posID     uint
	attempt   int
	pendingID string
	nextAt    time.Time
	last      ExitFailure // most recent failed attempt; zero until one fails
}

// ExitFailure describes the latest failed live exit attempt of an open
// position (CRY-34). The API reports it so the operator sees coins that
// are still on the venue after a stop / take-profit could not sell.
type ExitFailure struct {
	ExitReason   models.ExitReason `json:"exit_reason"`
	Reason       string            `json:"reason"`
	Detail       string            `json:"detail"`
	Attempt      int               `json:"attempt"`
	LastFailedAt time.Time         `json:"last_failed_at"`
	NextRetryAt  time.Time         `json:"next_retry_at"`
}

const (
	exitBackoffBase = 5 * time.Second
	exitBackoffMax  = 5 * time.Minute
)

func newExitTracker() *exitTracker {
	return &exitTracker{inflight: map[string]bool{}, state: map[string]*exitState{}}
}

// acquire claims the symbol's single SELL slot. false = a SELL is
// already in flight.
func (t *exitTracker) acquire(symbol string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inflight[symbol] {
		return false
	}
	t.inflight[symbol] = true
	return true
}

func (t *exitTracker) release(symbol string) {
	t.mu.Lock()
	delete(t.inflight, symbol)
	t.mu.Unlock()
}

// stateFor returns the state for this position, resetting it when the
// symbol has moved on to a new position.
func (t *exitTracker) stateFor(symbol string, posID uint) *exitState {
	st, ok := t.state[symbol]
	if !ok || st.posID != posID {
		st = &exitState{posID: posID}
		t.state[symbol] = st
	}
	return st
}

// ready reports whether a monitor exit may be attempted now.
func (t *exitTracker) ready(symbol string, posID uint, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inflight[symbol] {
		return false
	}
	return !now.Before(t.stateFor(symbol, posID).nextAt)
}

// fail records a failed attempt and schedules the next one. When
// pendingID is non-empty the order outcome is unknown and the next
// attempt must look it up rather than place a new order.
// f carries what failed (exit reason, fail reason, detail); fail fills
// in the attempt number and timestamps and keeps it as the last failure.
func (t *exitTracker) fail(symbol string, posID uint, pendingID string, f ExitFailure, now time.Time) (attempt int, nextAt time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.stateFor(symbol, posID)
	st.attempt++
	st.pendingID = pendingID
	backoff := exitBackoffBase << min(st.attempt-1, 10)
	if backoff > exitBackoffMax {
		backoff = exitBackoffMax
	}
	st.nextAt = now.Add(backoff)
	f.Attempt, f.LastFailedAt, f.NextRetryAt = st.attempt, now, st.nextAt
	st.last = f
	return st.attempt, st.nextAt
}

// lastFailure returns the latest failed exit attempt for this position.
// Read-only: unlike stateFor it never creates or resets state.
func (t *exitTracker) lastFailure(symbol string, posID uint) (ExitFailure, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.state[symbol]
	if !ok || st.posID != posID || st.attempt == 0 {
		return ExitFailure{}, false
	}
	return st.last, true
}

func (t *exitTracker) snapshot(symbol string, posID uint) exitState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return *t.stateFor(symbol, posID)
}

func (t *exitTracker) clear(symbol string) {
	t.mu.Lock()
	delete(t.state, symbol)
	t.mu.Unlock()
}

// errExitInFlight is returned when another SELL for the symbol is
// already being placed.
var errExitInFlight = errors.New("exit already in flight for symbol")

// ExitFailure reports the latest failed live exit attempt of an open
// position, if its exit is currently failing. Cleared once the position
// sells; a new position on the symbol starts clean.
func (e *Executor) ExitFailure(symbol string, posID uint) (ExitFailure, bool) {
	return e.exits.lastFailure(symbol, posID)
}

// exitClientOrderID is unique per position + attempt so a retry after a
// definite rejection is a new order, while an ambiguous attempt is
// re-queried under the same id. Fits Binance's 36-char limit.
func exitClientOrderID(posID uint, attempt int) string {
	return fmt.Sprintf("exit-%d-%d", posID, attempt)
}

// exitReady is the monitor's pre-check: in live mode it skips positions
// whose exit is in flight or backing off, before an audit row is
// written for the tick. Paper / dry-run are always ready.
func (e *Executor) exitReady(pos models.Position, now time.Time) bool {
	if e.mode != ModeLive {
		return true
	}
	return e.exits.ready(pos.Symbol, pos.ID, now)
}

// exitPosition executes a monitor-triggered exit. Paper / dry-run close
// at the quote price with the simulated fee. Live sells on the venue
// first and settles the DB from the actual fill; on any failure the
// position stays OPEN and the exit is retried with backoff.
func (e *Executor) exitPosition(ctx context.Context, pos models.Position, quote decimal.Decimal, reason models.ExitReason, auditID uint) (uint, error) {
	if e.mode != ModeLive {
		return e.closePosition(ctx, closeFillInput{
			Symbol:     pos.Symbol,
			ExitPrice:  quote,
			ExitQty:    pos.Quantity,
			ExitFee:    quote.Mul(pos.Quantity).Mul(e.feeRate),
			ExitReason: reason,
			AuditLogID: auditID,
			IsPaper:    e.mode == ModePaper,
		})
	}
	return e.liveExit(ctx, pos, quote, reason, auditID)
}

func (e *Executor) liveExit(ctx context.Context, pos models.Position, quote decimal.Decimal, reason models.ExitReason, auditID uint) (uint, error) {
	if e.provider == nil {
		return 0, e.exitFailed(ctx, pos, reason, auditID, "", "provider_unavailable", errors.New("execution provider not configured"))
	}
	if !e.exits.acquire(pos.Symbol) {
		return 0, errExitInFlight
	}
	defer e.exits.release(pos.Symbol)

	st := e.exits.snapshot(pos.Symbol, pos.ID)
	var resp execution.OrderResponse
	placed := false

	// An earlier attempt may have reached the venue without us seeing
	// the result. Look it up before selling again.
	if st.pendingID != "" {
		r, err := e.provider.GetOrder(ctx, pos.Symbol, st.pendingID)
		switch {
		case errors.Is(err, execution.ErrOrderNotFound):
			// Never placed — fall through to a fresh order.
		case err != nil:
			return 0, e.exitFailed(ctx, pos, reason, auditID, st.pendingID, "lookup_error", err)
		case !r.Status.IsTerminal():
			return 0, e.exitFailed(ctx, pos, reason, auditID, st.pendingID, "order_open",
				fmt.Errorf("exit order %s still %s", st.pendingID, r.Status))
		default:
			resp, placed = r, true
		}
	}

	if !placed {
		clientID := exitClientOrderID(pos.ID, st.attempt)
		r, err := e.provider.PlaceOrder(ctx, execution.OrderRequest{
			Symbol:        pos.Symbol,
			ClientOrderID: clientID,
			Side:          execution.SideSell,
			Type:          execution.OrderMarket,
			Quantity:      pos.Quantity,
		})
		if err != nil {
			pending := ""
			if errors.Is(err, execution.ErrAmbiguousPlacement) || errors.Is(err, execution.ErrPollTimeout) {
				pending = clientID
			}
			return 0, e.exitFailed(ctx, pos, reason, auditID, pending, providerFailReason(err), err)
		}
		resp = r
	}

	if resp.FilledQty.IsZero() {
		return 0, e.exitFailed(ctx, pos, reason, auditID, "", "exchange:"+string(resp.Status),
			fmt.Errorf("exit order %s ended %s with no fill", resp.ClientOrderID, resp.Status))
	}
	warnPartialFill("SELL", resp)
	if !quote.IsZero() {
		drift := resp.AvgFillPrice.Sub(quote).Div(quote).Mul(decimal.NewFromInt(10000))
		log.Printf("[executor] live %s fill: sym=%s qty=%s avg=%s trigger_quote=%s drift=%s bps",
			reason, pos.Symbol, resp.FilledQty, resp.AvgFillPrice, quote, drift.StringFixed(1))
	}

	exitFee, exitFeeUnvalued := feeFromFillsValued(resp.Fills, quoteAsset, baseAssetOf(pos.Symbol, quoteAsset), tickerPricer(ctx, e.provider))
	if exitFeeUnvalued {
		log.Printf("[executor] WARN: %s exit %s — commission in an asset that can't be valued in %s (e.g. BNB); ExitFee may understate cost",
			reason, resp.ClientOrderID, quoteAsset)
	}
	tradeID, err := e.closePosition(ctx, closeFillInput{
		Symbol:          pos.Symbol,
		ExitPrice:       resp.AvgFillPrice,
		ExitQty:         resp.FilledQty,
		ExitFee:         exitFee,
		DustQty:         minQtyFor(ctx, e.provider, pos.Symbol),
		ExitReason:      reason,
		AuditLogID:      auditID,
		ExchangeOrderID: resp.ExchangeOrderID,
		ClientOrderID:   resp.ClientOrderID,
	})
	// The venue fill happened either way — clear retry state so the
	// next tick (e.g. a partial-fill remainder) starts fresh.
	e.exits.clear(pos.Symbol)
	if errors.Is(err, ErrAlreadyClosed) {
		log.Printf("[executor] live %s SELL fill orphaned by prior close: sym=%s qty=%s price=%s client=%s",
			reason, pos.Symbol, resp.FilledQty, resp.AvgFillPrice, resp.ClientOrderID)
	}
	return tradeID, err
}

// exitFailed records a failed live exit attempt: schedules the retry,
// tags the attempt's audit row, logs, and pushes a WS alert. The
// position is untouched (still OPEN).
func (e *Executor) exitFailed(ctx context.Context, pos models.Position, reason models.ExitReason, auditID uint, pendingID, failReason string, cause error) error {
	attempt, nextAt := e.exits.fail(pos.Symbol, pos.ID, pendingID,
		ExitFailure{ExitReason: reason, Reason: failReason, Detail: cause.Error()}, time.Now())
	e.linkAuditLog(ctx, auditID, nil, "execution_failed:"+failReason)
	log.Printf("[executor] live %s exit FAILED sym=%s qty=%s reason=%s attempt=%d retry_at=%s: %v",
		reason, pos.Symbol, pos.Quantity, failReason, attempt, nextAt.Format(time.RFC3339), cause)
	if e.notifier != nil {
		e.notifier.Broadcast(map[string]any{
			"type": "position.exit_failed",
			"data": map[string]any{
				"symbol":        pos.Symbol,
				"position_id":   pos.ID,
				"exit_reason":   reason,
				"reason":        failReason,
				"detail":        cause.Error(),
				"attempt":       attempt,
				"next_retry_at": nextAt,
			},
		})
	}
	return fmt.Errorf("%s: %w", failReason, cause)
}
