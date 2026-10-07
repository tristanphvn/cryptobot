package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cryptobot/internal/api/middleware"
	"cryptobot/internal/approval"
	"cryptobot/internal/database/models"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// fakeSubmitter satisfies handlers.Submitter without a real executor.
// It also lets tests force an error from Submit to exercise the
// MarkFailed path.
type fakeSubmitter struct {
	trade uint
	err   error
	calls int
}

func (f *fakeSubmitter) Submit(_ *gin.Context, _ uint) (uint, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	return f.trade, nil
}

// fakeNotifier records broadcasts so tests can assert on emitted WS
// messages without spinning up a real Hub.
type fakeNotifier struct {
	msgs []map[string]any
}

func (f *fakeNotifier) Broadcast(m map[string]any) { f.msgs = append(f.msgs, m) }

const testToken = "test-bearer-123"

func setupRouter(t *testing.T, submitter Submitter, notifier Notifier) (*gin.Engine, *approval.StateMachine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&models.PendingSignal{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	sm := approval.NewStateMachine(db, approval.Config{TTL: 30 * time.Second, QueueMax: 5})
	handler := NewApprovalHandler(sm, submitter, notifier)

	r := gin.New()
	g := r.Group("/api/v1/signals", middleware.Bearer(testToken))
	g.GET("/pending", handler.ListPending)
	g.POST("/:id/approve", handler.Approve)
	g.POST("/:id/reject", handler.Reject)
	return r, sm, db
}

func seedPending(t *testing.T, sm *approval.StateMachine, auditID uint) *models.PendingSignal {
	t.Helper()
	ps, err := sm.Enqueue(context.Background(), approval.EnqueueInput{
		Symbol:      "BTCUSDT",
		Interval:    "1h",
		AuditLogID:  auditID,
		Action:      "BUY",
		Confidence:  0.85,
		Reasoning:   "test",
		ProposedSL:  decimal.NewFromFloat(73000),
		ProposedTP:  decimal.NewFromFloat(80000),
		MarketPrice: decimal.NewFromFloat(77000),
	})
	if err != nil {
		t.Fatalf("seed enqueue: %v", err)
	}
	return ps
}

func doReq(r *gin.Engine, method, path, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestListPendingRejectsMissingBearer(t *testing.T) {
	r, _, _ := setupRouter(t, &fakeSubmitter{}, &fakeNotifier{})
	w := doReq(r, "GET", "/api/v1/signals/pending", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
}

func TestListPendingRejectsWrongBearer(t *testing.T) {
	r, _, _ := setupRouter(t, &fakeSubmitter{}, &fakeNotifier{})
	w := doReq(r, "GET", "/api/v1/signals/pending", "not-the-right-token", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", w.Code)
	}
}

func TestListPendingHappyPath(t *testing.T) {
	r, sm, _ := setupRouter(t, &fakeSubmitter{}, &fakeNotifier{})
	seedPending(t, sm, 1)
	seedPending(t, sm, 2)

	w := doReq(r, "GET", "/api/v1/signals/pending", testToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Pending []models.PendingSignal `json:"pending"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Pending) != 2 {
		t.Fatalf("expected 2 pending, got %d", len(resp.Pending))
	}
}

func TestApproveHappyPathSetsSubmittedAndBroadcasts(t *testing.T) {
	notifier := &fakeNotifier{}
	submitter := &fakeSubmitter{trade: 42}
	r, sm, db := setupRouter(t, submitter, notifier)
	ps := seedPending(t, sm, 1)

	w := doReq(r, "POST", "/api/v1/signals/"+itoa(ps.ID)+"/approve", testToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	if submitter.calls != 1 {
		t.Fatalf("submitter calls=%d want 1", submitter.calls)
	}
	// State must be submitted, not just approved.
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalSubmitted {
		t.Fatalf("status=%s want submitted", got.Status)
	}
	if got.TradeID == nil || *got.TradeID != 42 {
		t.Fatalf("trade_id=%v want 42", got.TradeID)
	}
	// Exactly one signal.approved broadcast.
	count := 0
	for _, m := range notifier.msgs {
		if m["type"] == "signal.approved" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("approved broadcasts=%d want 1; all=%v", count, notifier.msgs)
	}
}

func TestApproveAlreadyDecidedReturns409(t *testing.T) {
	r, sm, _ := setupRouter(t, &fakeSubmitter{trade: 1}, &fakeNotifier{})
	ps := seedPending(t, sm, 1)

	if w := doReq(r, "POST", "/api/v1/signals/"+itoa(ps.ID)+"/approve", testToken, ""); w.Code != 200 {
		t.Fatalf("first approve status=%d", w.Code)
	}
	w := doReq(r, "POST", "/api/v1/signals/"+itoa(ps.ID)+"/approve", testToken, "")
	if w.Code != http.StatusConflict {
		t.Fatalf("second approve status=%d want 409", w.Code)
	}
}

func TestApproveExpiredReturns410(t *testing.T) {
	r, sm, db := setupRouter(t, &fakeSubmitter{trade: 1}, &fakeNotifier{})
	ps := seedPending(t, sm, 1)
	// Back-date the row so the atomic UPDATE filters it out.
	db.Model(&models.PendingSignal{}).Where("id = ?", ps.ID).Update("expires_at", time.Now().Add(-time.Second))

	w := doReq(r, "POST", "/api/v1/signals/"+itoa(ps.ID)+"/approve", testToken, "")
	if w.Code != http.StatusGone {
		t.Fatalf("status=%d want 410", w.Code)
	}
}

func TestApproveNonExistentReturns404(t *testing.T) {
	r, _, _ := setupRouter(t, &fakeSubmitter{trade: 1}, &fakeNotifier{})
	w := doReq(r, "POST", "/api/v1/signals/9999/approve", testToken, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", w.Code)
	}
}

func TestApproveSubmitterFailureMarksFailedAnd502(t *testing.T) {
	r, sm, db := setupRouter(t, &fakeSubmitter{err: errors.New("binance 500")}, &fakeNotifier{})
	ps := seedPending(t, sm, 1)

	w := doReq(r, "POST", "/api/v1/signals/"+itoa(ps.ID)+"/approve", testToken, "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", w.Code)
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalFailed {
		t.Fatalf("status=%s want failed", got.Status)
	}
}

func TestRejectHappyPath(t *testing.T) {
	notifier := &fakeNotifier{}
	r, sm, db := setupRouter(t, &fakeSubmitter{trade: 1}, notifier)
	ps := seedPending(t, sm, 1)

	w := doReq(r, "POST", "/api/v1/signals/"+itoa(ps.ID)+"/reject", testToken, `{"reason":"too risky"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", w.Code, w.Body.String())
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalRejected {
		t.Fatalf("status=%s want rejected", got.Status)
	}
	if got.RejectedReason != "too risky" {
		t.Fatalf("rejected_reason=%q", got.RejectedReason)
	}
	hasReject := false
	for _, m := range notifier.msgs {
		if m["type"] == "signal.rejected" {
			hasReject = true
		}
	}
	if !hasReject {
		t.Fatalf("missing signal.rejected broadcast in %v", notifier.msgs)
	}
}

func TestApproveBadIDReturns400(t *testing.T) {
	r, _, _ := setupRouter(t, &fakeSubmitter{trade: 1}, &fakeNotifier{})
	w := doReq(r, "POST", "/api/v1/signals/abc/approve", testToken, "")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", w.Code)
	}
}

// itoa is a tiny helper that avoids importing strconv at every call-site.
func itoa(u uint) string {
	if u == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}
