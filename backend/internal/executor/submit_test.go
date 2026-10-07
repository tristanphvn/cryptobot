package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"cryptobot/internal/approval"
	"cryptobot/internal/database/models"
	"cryptobot/internal/execution"
	"cryptobot/internal/riskmanager"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// fakeProvider records calls and returns canned responses. Used to
// exercise the executor's wiring without a real httptest server.
type fakeProvider struct {
	mu           sync.Mutex
	placeCalls   []execution.OrderRequest
	placeResp    execution.OrderResponse
	placeErr     error
	getOrderResp execution.OrderResponse
	getOrderErr  error
	filters      execution.SymbolFilters
	getOrderIDs  []string
	tickerPrice  decimal.Decimal
	tickerErr    error
	tickerCalls  []string
}

func (f *fakeProvider) PlaceOrder(_ context.Context, req execution.OrderRequest) (execution.OrderResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.placeCalls = append(f.placeCalls, req)
	if f.placeErr != nil {
		return execution.OrderResponse{}, f.placeErr
	}
	return f.placeResp, nil
}

func (f *fakeProvider) GetOrder(_ context.Context, _, clientOrderID string) (execution.OrderResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getOrderIDs = append(f.getOrderIDs, clientOrderID)
	return f.getOrderResp, f.getOrderErr
}

func (f *fakeProvider) GetAccountBalance(_ context.Context) (map[string]execution.AccountBalance, error) {
	return map[string]execution.AccountBalance{}, nil
}

func (f *fakeProvider) GetSymbolFilters(_ context.Context, _ string) (execution.SymbolFilters, error) {
	return f.filters, nil
}

func (f *fakeProvider) GetTickerPrice(_ context.Context, symbol string) (decimal.Decimal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tickerCalls = append(f.tickerCalls, symbol)
	if f.tickerErr != nil {
		return decimal.Zero, f.tickerErr
	}
	return f.tickerPrice, nil
}

type fakeNotifier struct {
	mu     sync.Mutex
	events []map[string]any
}

func (f *fakeNotifier) Broadcast(m map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, m)
}

func (f *fakeNotifier) byType(t string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, e := range f.events {
		if e["type"] == t {
			out = append(out, e)
		}
	}
	return out
}

func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.AutoMigrate(
		&models.PendingSignal{},
		&models.Trade{},
		&models.Position{},
		&models.AuditLog{},
	); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// In-memory shared-cache SQLite cannot run two write transactions
	// concurrently (SQLITE_LOCKED, no busy wait). Postgres serialises
	// them with row locks; one connection reproduces that ordering so
	// transactional close paths (settleClose) can be exercised under
	// concurrency.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("sql db: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return db
}

// newSubmittableExecutor builds an executor wired to a fake provider
// + notifier with a freshly-approved pending_signal already in the
// `executing` state. Returns everything the test will need to assert.
func newSubmittableExecutor(t *testing.T, provider execution.ExecutionProvider, action string, maxSlippageBps int64) (*Executor, *gorm.DB, *approval.StateMachine, *fakeNotifier, *models.PendingSignal) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: 30 * time.Second, QueueMax: 5})

	ps, err := sm.Enqueue(context.Background(), approval.EnqueueInput{
		Symbol:      "BTCUSDT",
		Interval:    "1h",
		AuditLogID:  1,
		Action:      action,
		Confidence:  0.9,
		Reasoning:   "test",
		MarketPrice: decimal.NewFromInt(70000),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := sm.Approve(context.Background(), ps.ID, approval.Decision{By: "op", Source: "api"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := sm.MarkExecuting(context.Background(), ps.ID); err != nil {
		t.Fatalf("mark executing: %v", err)
	}

	// Seed an audit_log row so linkAuditLog has something to patch.
	db.Create(&models.AuditLog{
		ID: 1, Symbol: "BTCUSDT", Interval: "1h", Prompt: "p",
		RawResponse: "r", Action: action, Confidence: 0.9,
		PromptSentAt: time.Now(), ResponseAt: time.Now(),
	})

	risk := riskmanager.New(db, riskmanager.Config{
		MaxPositionPct: 2.0, DailyDrawdownPct: 5.0, MaxOpenPositions: 3,
	})

	notifier := &fakeNotifier{}
	exec := NewExecutor(db, nil, risk, ModeLive, sm).
		WithNotifier(notifier).
		WithProvider(provider, maxSlippageBps)

	// Reload the pending signal so the test sees the executing state.
	var refreshed models.PendingSignal
	db.First(&refreshed, ps.ID)
	return exec, db, sm, notifier, &refreshed
}

// ginCtx is the minimal Gin context the executor's Submit signature
// requires; we never invoke Gin middleware here.
func ginCtx() *gin.Context {
	return &gin.Context{Request: nil}
}

// ─────────────────────────────────────────────────────────────────────
// submitOpen — happy path & wiring
// ─────────────────────────────────────────────────────────────────────

func TestSubmit_BuyHappyPath_TradeUsesActualFill(t *testing.T) {
	provider := &fakeProvider{
		placeResp: execution.OrderResponse{
			ClientOrderID:   "cry5-1",
			ExchangeOrderID: 555,
			Symbol:          "BTCUSDT",
			Side:            execution.SideBuy,
			Status:          execution.StatusFilled,
			FilledQty:       decimal.RequireFromString("0.001"),
			AvgFillPrice:    decimal.RequireFromString("70014"), // ~2 bps drift, within 50 bps cap
			CumQuoteQty:     decimal.NewFromInt(70),
		},
	}
	exec, db, _, notifier, ps := newSubmittableExecutor(t, provider, "BUY", 50)

	tradeID, err := exec.submitOpen(context.Background(), ps)
	if err != nil {
		t.Fatalf("submitOpen: %v", err)
	}
	if tradeID == 0 {
		t.Fatal("expected non-zero trade id")
	}
	var trade models.Trade
	if err := db.First(&trade, tradeID).Error; err != nil {
		t.Fatalf("load trade: %v", err)
	}
	if trade.Status != models.TradeStatusOpen {
		t.Fatalf("trade.status=%s want OPEN", trade.Status)
	}
	if !trade.EntryPrice.Equal(decimal.RequireFromString("70014")) {
		t.Fatalf("entry_price=%s want 70014 (binance fill, NOT signal snapshot)", trade.EntryPrice)
	}
	if !trade.Quantity.Equal(decimal.RequireFromString("0.001")) {
		t.Fatalf("qty=%s want 0.001", trade.Quantity)
	}
	if trade.ExchangeOrderID != 555 || trade.ClientOrderID != "cry5-1" {
		t.Fatalf("exchange ids missing: orderID=%d clientID=%s", trade.ExchangeOrderID, trade.ClientOrderID)
	}
	if got := provider.placeCalls[0].ClientOrderID; got != "cry5-1" {
		t.Fatalf("ClientOrderID sent=%q want cry5-1", got)
	}
	if len(notifier.byType("signal.failed")) != 0 {
		t.Fatal("did not expect signal.failed event on happy path")
	}
}

// ─────────────────────────────────────────────────────────────────────
// Slippage
// ─────────────────────────────────────────────────────────────────────

func TestSubmit_SlippageExceeded_MarksFailed_AndEmitsEvent(t *testing.T) {
	// expected=70000, actual=70700 → drift = 100 bps; cap=50 → exceeds.
	provider := &fakeProvider{
		placeResp: execution.OrderResponse{
			ClientOrderID:   "cry5-1",
			ExchangeOrderID: 1,
			Status:          execution.StatusFilled,
			FilledQty:       decimal.RequireFromString("0.001"),
			AvgFillPrice:    decimal.RequireFromString("70700"),
		},
	}
	exec, db, _, notifier, ps := newSubmittableExecutor(t, provider, "BUY", 50)

	_, err := exec.submitOpen(context.Background(), ps)
	if err == nil || !strings.Contains(err.Error(), "slippage_exceeded") {
		t.Fatalf("err=%v want slippage_exceeded", err)
	}

	var trades []models.Trade
	db.Where("symbol = ?", "BTCUSDT").Find(&trades)
	if len(trades) != 1 {
		t.Fatalf("expected exactly one trade row (the FAILED one), got %d", len(trades))
	}
	if trades[0].Status != models.TradeStatusFailed {
		t.Fatalf("trade.status=%s want FAILED", trades[0].Status)
	}
	if !strings.HasPrefix(trades[0].Error, "slippage_exceeded") {
		t.Fatalf("trade.error=%q want prefix slippage_exceeded", trades[0].Error)
	}
	failedEvents := notifier.byType("signal.failed")
	if len(failedEvents) != 1 {
		t.Fatalf("signal.failed events=%d want 1", len(failedEvents))
	}
	data, _ := failedEvents[0]["data"].(map[string]any)
	if data["reason"] != "slippage_exceeded" {
		t.Fatalf("event reason=%v want slippage_exceeded", data["reason"])
	}

	// Position must NOT have been created on a slippage-failed open.
	var positions []models.Position
	db.Where("symbol = ?", "BTCUSDT").Find(&positions)
	if len(positions) != 0 {
		t.Fatalf("expected 0 positions after FAILED open, got %d", len(positions))
	}
}

// ─────────────────────────────────────────────────────────────────────
// Provider errors map to specific reasons
// ─────────────────────────────────────────────────────────────────────

func TestSubmit_ProviderError_RecordsFailedTrade(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantReason string
	}{
		{"insufficient_balance", fmt.Errorf("%w: -2010", execution.ErrInsufficientBalance), "insufficient_balance"},
		{"signature_rejected", fmt.Errorf("%w: -1022", execution.ErrSignatureRejected), "signature_rejected"},
		{"poll_timeout", execution.ErrPollTimeout, "poll_timeout"},
		{"ambiguous_placement", fmt.Errorf("%w: 502 upstream", execution.ErrAmbiguousPlacement), "ambiguous_placement"},
		{"unknown", errors.New("something else"), "exchange_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &fakeProvider{placeErr: tc.err}
			exec, db, _, notifier, ps := newSubmittableExecutor(t, provider, "BUY", 50)

			_, err := exec.submitOpen(context.Background(), ps)
			if err == nil {
				t.Fatal("expected err")
			}
			var trades []models.Trade
			db.Where("symbol = ?", "BTCUSDT").Find(&trades)
			if len(trades) != 1 || trades[0].Status != models.TradeStatusFailed {
				t.Fatalf("expected 1 FAILED trade, got %+v", trades)
			}
			if !strings.HasPrefix(trades[0].Error, tc.wantReason) {
				t.Fatalf("trade.error=%q want prefix %q", trades[0].Error, tc.wantReason)
			}
			ev := notifier.byType("signal.failed")
			if len(ev) != 1 {
				t.Fatalf("signal.failed events=%d want 1", len(ev))
			}
			if data, _ := ev[0]["data"].(map[string]any); data["reason"] != tc.wantReason {
				t.Fatalf("event reason=%v want %q", data["reason"], tc.wantReason)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// Idempotency — same ClientOrderID across retries
// ─────────────────────────────────────────────────────────────────────

func TestSubmit_ClientOrderIDIsDeterministic(t *testing.T) {
	provider := &fakeProvider{
		placeResp: execution.OrderResponse{
			Status:       execution.StatusFilled,
			FilledQty:    decimal.RequireFromString("0.001"),
			AvgFillPrice: decimal.NewFromInt(70000),
		},
	}
	exec, _, _, _, ps := newSubmittableExecutor(t, provider, "BUY", 50)

	// First Submit creates a Trade + Position.
	_, _ = exec.submitOpen(context.Background(), ps)
	// Simulate a retry of submitOpen against the SAME pending_signal.
	// (In production the handler guards via state machine status — this
	// test only verifies the deterministic ID is produced both times.)
	_, _ = exec.submitOpen(context.Background(), ps)
	if len(provider.placeCalls) != 2 {
		t.Fatalf("placeCalls=%d want 2", len(provider.placeCalls))
	}
	if provider.placeCalls[0].ClientOrderID != provider.placeCalls[1].ClientOrderID {
		t.Fatalf("ClientOrderID drifted: %q vs %q",
			provider.placeCalls[0].ClientOrderID, provider.placeCalls[1].ClientOrderID)
	}
	if !strings.HasPrefix(provider.placeCalls[0].ClientOrderID, "cry5-") {
		t.Fatalf("ClientOrderID=%q want cry5- prefix", provider.placeCalls[0].ClientOrderID)
	}
}

// ─────────────────────────────────────────────────────────────────────
// submitClose
// ─────────────────────────────────────────────────────────────────────

func TestSubmit_SellHappyPath_RecordsActualExitPrice(t *testing.T) {
	provider := &fakeProvider{
		placeResp: execution.OrderResponse{
			ClientOrderID:   "cry5-1",
			ExchangeOrderID: 777,
			Status:          execution.StatusFilled,
			FilledQty:       decimal.RequireFromString("0.001"),
			AvgFillPrice:    decimal.NewFromInt(71000), // ~143 bps drift
		},
	}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "SELL", 200) // cap 200 bps → no slippage trip

	// Pre-seed an OPEN trade + position so submitClose has something to close.
	openTrade := models.Trade{
		Symbol: "BTCUSDT", Side: models.TradeSideBuy, Status: models.TradeStatusOpen,
		EntryPrice: decimal.NewFromInt(70000),
		Quantity:   decimal.RequireFromString("0.001"),
		EntryAt:    time.Now(),
	}
	db.Create(&openTrade)
	pos := &models.Position{
		Symbol: "BTCUSDT", Side: models.TradeSideBuy,
		Quantity:      decimal.RequireFromString("0.001"),
		AvgEntryPrice: decimal.NewFromInt(70000),
		IsOpen:        true,
	}
	db.Create(pos)

	tradeID, err := exec.submitClose(context.Background(), ps, pos)
	if err != nil {
		t.Fatalf("submitClose: %v", err)
	}
	var got models.Trade
	db.First(&got, tradeID)
	if got.Status != models.TradeStatusClosed {
		t.Fatalf("trade.status=%s want CLOSED", got.Status)
	}
	if !got.ExitPrice.Equal(decimal.NewFromInt(71000)) {
		t.Fatalf("exit_price=%s want 71000 (binance fill)", got.ExitPrice)
	}
	want := decimal.RequireFromString("1") // (71000-70000)*0.001
	if !got.PnL.Equal(want) {
		t.Fatalf("pnl=%s want %s", got.PnL, want)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Provider-not-wired guard
// ─────────────────────────────────────────────────────────────────────

func TestSubmit_NoProvider_LiveMode_FailsClearly(t *testing.T) {
	db := newTestDB(t)
	sm := approval.NewStateMachine(db, approval.Config{TTL: 30 * time.Second, QueueMax: 5})
	risk := riskmanager.New(db, riskmanager.Config{MaxPositionPct: 2.0, MaxOpenPositions: 3})
	// Note: NO WithProvider call.
	exec := NewExecutor(db, nil, risk, ModeLive, sm)

	ps, _ := sm.Enqueue(context.Background(), approval.EnqueueInput{
		Symbol: "BTCUSDT", AuditLogID: 1, Action: "BUY", MarketPrice: decimal.NewFromInt(70000),
	})
	_, _ = sm.Approve(context.Background(), ps.ID, approval.Decision{By: "op", Source: "api"})
	_ = sm.MarkExecuting(context.Background(), ps.ID)
	var refreshed models.PendingSignal
	db.First(&refreshed, ps.ID)

	_, err := exec.submitOpen(context.Background(), &refreshed)
	if err == nil || !strings.Contains(err.Error(), "provider") {
		t.Fatalf("err=%v want provider-related failure", err)
	}
}

// ─────────────────────────────────────────────────────────────────────
// CRY-9 — fee accounting + min-return gate
// ─────────────────────────────────────────────────────────────────────

func TestSubmit_BuyEntryFee_SummedFromUSDTFills(t *testing.T) {
	provider := &fakeProvider{
		placeResp: execution.OrderResponse{
			ClientOrderID:   "cry5-1",
			ExchangeOrderID: 555,
			Symbol:          "BTCUSDT",
			Side:            execution.SideBuy,
			Status:          execution.StatusFilled,
			FilledQty:       decimal.RequireFromString("0.001"),
			AvgFillPrice:    decimal.NewFromInt(70000),
			Fills: []execution.Fill{
				{Commission: decimal.RequireFromString("0.026"), CommissionAsset: "USDT"},
				{Commission: decimal.RequireFromString("0.026"), CommissionAsset: "USDT"},
			},
		},
	}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "BUY", 100)

	tradeID, err := exec.submitOpen(context.Background(), ps)
	if err != nil {
		t.Fatalf("submitOpen: %v", err)
	}
	var trade models.Trade
	db.First(&trade, tradeID)
	want := decimal.RequireFromString("0.052")
	if !trade.EntryFee.Equal(want) {
		t.Fatalf("entry_fee=%s want %s (sum of USDT fills)", trade.EntryFee, want)
	}
	if trade.FeeCurrency != "USDT" {
		t.Fatalf("fee_currency=%s want USDT", trade.FeeCurrency)
	}
}

// CRY-35: BNB commission is valued via the ticker; when the ticker is
// unavailable it stays unvalued (fee 0 + warning) and the open still settles.
func TestSubmit_BuyEntryFee_BNBUnvaluedWithoutTicker(t *testing.T) {
	provider := &fakeProvider{
		tickerErr: errors.New("ticker unavailable"),
		placeResp: execution.OrderResponse{
			ClientOrderID:   "cry5-1",
			ExchangeOrderID: 555,
			Status:          execution.StatusFilled,
			FilledQty:       decimal.RequireFromString("0.001"),
			AvgFillPrice:    decimal.NewFromInt(70000),
			Fills: []execution.Fill{
				{Commission: decimal.RequireFromString("0.0001"), CommissionAsset: "BNB"},
			},
		},
	}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "BUY", 100)

	tradeID, err := exec.submitOpen(context.Background(), ps)
	if err != nil {
		t.Fatalf("submitOpen: %v", err)
	}
	var trade models.Trade
	db.First(&trade, tradeID)
	if !trade.EntryFee.IsZero() {
		t.Fatalf("entry_fee=%s want 0 (BNB unvalued without a ticker price)", trade.EntryFee)
	}
}

func TestSubmit_SellNetPnLDeductsBothLegs(t *testing.T) {
	// Open at 70000, close at 71000 — gross PnL = 1.0 USDT.
	// Entry fee 0.07, exit fee 0.071, net = 1.0 - 0.07 - 0.071 = 0.859
	provider := &fakeProvider{
		placeResp: execution.OrderResponse{
			ClientOrderID:   "cry5-1",
			ExchangeOrderID: 777,
			Status:          execution.StatusFilled,
			FilledQty:       decimal.RequireFromString("0.001"),
			AvgFillPrice:    decimal.NewFromInt(71000),
			Fills: []execution.Fill{
				{Commission: decimal.RequireFromString("0.071"), CommissionAsset: "USDT"},
			},
		},
	}
	exec, db, _, _, ps := newSubmittableExecutor(t, provider, "SELL", 200)

	openTrade := models.Trade{
		Symbol: "BTCUSDT", Side: models.TradeSideBuy, Status: models.TradeStatusOpen,
		EntryPrice: decimal.NewFromInt(70000),
		Quantity:   decimal.RequireFromString("0.001"),
		EntryCost:  decimal.NewFromInt(70),
		EntryFee:   decimal.RequireFromString("0.07"),
		EntryAt:    time.Now(),
	}
	db.Create(&openTrade)
	pos := &models.Position{
		Symbol: "BTCUSDT", Side: models.TradeSideBuy,
		Quantity:      decimal.RequireFromString("0.001"),
		AvgEntryPrice: decimal.NewFromInt(70000),
		IsOpen:        true,
	}
	db.Create(pos)

	tradeID, err := exec.submitClose(context.Background(), ps, pos)
	if err != nil {
		t.Fatalf("submitClose: %v", err)
	}
	var got models.Trade
	db.First(&got, tradeID)
	wantNet := decimal.RequireFromString("0.859")
	if !got.NetPnL.Equal(wantNet) {
		t.Fatalf("net_pnl=%s want %s (= pnl 1.0 - entry_fee 0.07 - exit_fee 0.071)", got.NetPnL, wantNet)
	}
	if !got.PnL.Equal(decimal.RequireFromString("1")) {
		t.Fatalf("pnl=%s want 1 (gross unchanged)", got.PnL)
	}
	if !got.ExitFee.Equal(decimal.RequireFromString("0.071")) {
		t.Fatalf("exit_fee=%s want 0.071", got.ExitFee)
	}
}

func TestBelowMinReturn_GatingMath(t *testing.T) {
	exec := &Executor{minExpectedReturnBps: 30}
	cases := []struct {
		name        string
		tp, price   float64
		wantBlocked bool
	}{
		{"upside above cap (50 bps)", 100.5, 100, false},
		{"upside at cap (30 bps)", 100.3, 100, false},
		{"upside below cap (20 bps)", 100.2, 100, true},
		{"no upside", 100, 100, true},
		{"target below price", 99, 100, true},
		{"missing tp", 0, 100, true},
		{"missing price", 100.5, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blocked, bps := exec.belowMinReturn(c.tp, c.price)
			if blocked != c.wantBlocked {
				t.Fatalf("blocked=%v want %v (computed bps=%d)", blocked, c.wantBlocked, bps)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────────
// Slippage math
// ─────────────────────────────────────────────────────────────────────

func TestSlippageExceeded_DriftMath(t *testing.T) {
	exec := &Executor{maxSlippageBps: 50}
	// 70000 → 70350 = exactly 50 bps. NOT exceeded (strictly greater than).
	ok, bps := exec.slippageExceeded(decimal.NewFromInt(70000), decimal.NewFromInt(70350))
	if ok {
		t.Fatalf("50 bps drift should equal cap exactly, not exceed (got bps=%s)", bps)
	}
	// 70000 → 70351 = 50.14 bps → exceeds.
	ok, _ = exec.slippageExceeded(decimal.NewFromInt(70000), decimal.NewFromInt(70351))
	if !ok {
		t.Fatal("51 bps drift should exceed 50 bps cap")
	}
	// Downside drift is symmetric.
	ok, _ = exec.slippageExceeded(decimal.NewFromInt(70000), decimal.NewFromInt(69500))
	if !ok {
		t.Fatal("downside drift > 50 bps should also trip")
	}
	// Zero cap disables the check entirely.
	zero := &Executor{maxSlippageBps: 0}
	ok, _ = zero.slippageExceeded(decimal.NewFromInt(1), decimal.NewFromInt(1000))
	if ok {
		t.Fatal("zero cap should disable slippage check")
	}
}

// Avoid unused-import errors for fmt/Gin in trimmed builds.
var _ = gin.Default
var _ = fmt.Sprintf
