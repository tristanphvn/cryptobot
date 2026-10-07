package approval

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cryptobot/internal/database/models"

	"github.com/glebarez/sqlite"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

// newTestDB returns an in-memory sqlite DB with PendingSignal migrated.
//
// The DSN uses shared-cache mode so multiple gorm connections see the
// same in-memory database — required for the concurrent-approve test
// where goroutines may each acquire their own connection. Per-test
// isolation comes from a unique DB name derived from the test name.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + t.Name() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.PendingSignal{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func sampleInput(auditID uint) EnqueueInput {
	return EnqueueInput{
		Symbol:      "BTCUSDT",
		Interval:    "1h",
		AuditLogID:  auditID,
		Action:      "BUY",
		Confidence:  0.85,
		Reasoning:   "test",
		ProposedSL:  decimal.NewFromFloat(73000),
		ProposedTP:  decimal.NewFromFloat(80000),
		MarketPrice: decimal.NewFromFloat(77000),
	}
}

func TestEnqueueCreatesRowWithTTL(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})

	ps, err := sm.Enqueue(context.Background(), sampleInput(1))
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if ps.ID == 0 {
		t.Fatal("expected non-zero id")
	}
	if ps.Status != models.PendingSignalPending {
		t.Fatalf("status=%s want pending", ps.Status)
	}
	if time.Until(ps.ExpiresAt) <= 0 {
		t.Fatalf("expires_at should be in the future, got %s", ps.ExpiresAt)
	}
}

func TestEnqueueIdempotentOnAuditLogID(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})

	first, err := sm.Enqueue(context.Background(), sampleInput(7))
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	second, err := sm.Enqueue(context.Background(), sampleInput(7))
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if first.ID != second.ID {
		t.Fatalf("expected same id (idempotent on audit_log_id) — got %d vs %d", first.ID, second.ID)
	}

	var count int64
	db.Model(&models.PendingSignal{}).Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

func TestEnqueueRespectsQueueMax(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 3})

	for i := uint(1); i <= 3; i++ {
		if _, err := sm.Enqueue(context.Background(), sampleInput(i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	_, err := sm.Enqueue(context.Background(), sampleInput(4))
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("4th enqueue want ErrQueueFull, got %v", err)
	}
}

func TestApproveSetsStatusAndDecidedFields(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})

	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	updated, err := sm.Approve(context.Background(), ps.ID, Decision{
		By:        "operator-x",
		Source:    "api",
		IP:        "10.0.0.5",
		UserAgent: "curl/8.0",
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if updated.Status != models.PendingSignalApproved {
		t.Fatalf("status=%s want approved", updated.Status)
	}
	if updated.DecidedBy != "operator-x" {
		t.Fatalf("decided_by=%s want operator-x", updated.DecidedBy)
	}
	if updated.ApprovalSource != "api" || updated.ApprovalIP != "10.0.0.5" || updated.ApprovalUserAgent != "curl/8.0" {
		t.Fatalf("provenance not persisted: source=%q ip=%q ua=%q",
			updated.ApprovalSource, updated.ApprovalIP, updated.ApprovalUserAgent)
	}
	if updated.DecidedAt == nil {
		t.Fatal("decided_at should be set")
	}
}

func TestApproveTwiceReturnsErrNotPending(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))

	if _, err := sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"}); err != nil {
		t.Fatalf("first approve: %v", err)
	}
	_, err := sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("second approve want ErrNotPending, got %v", err)
	}
}

func TestApproveNonExistentReturnsErrNotFound(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	_, err := sm.Approve(context.Background(), 9999, Decision{By: "op", Source: "api"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestApproveExpiredReturnsErrExpired(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 1 * time.Second, QueueMax: 5})

	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	// Force expiration deterministically — just back-date the row.
	db.Model(&models.PendingSignal{}).
		Where("id = ?", ps.ID).
		Update("expires_at", time.Now().Add(-time.Second))

	_, err := sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestRejectSetsReason(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))

	updated, err := sm.Reject(context.Background(), ps.ID, "low confidence", Decision{By: "op", Source: "api"})
	if err != nil {
		t.Fatalf("reject: %v", err)
	}
	if updated.Status != models.PendingSignalRejected {
		t.Fatalf("status=%s want rejected", updated.Status)
	}
	if updated.RejectedReason != "low confidence" {
		t.Fatalf("rejected_reason=%q want %q", updated.RejectedReason, "low confidence")
	}
}

func TestSubmittedFlowRequiresExecuting(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))

	// MarkSubmitted from pending → not allowed.
	if err := sm.MarkSubmitted(context.Background(), ps.ID, 42); !errors.Is(err, ErrNotPending) {
		t.Fatalf("MarkSubmitted from pending: want ErrNotPending, got %v", err)
	}

	_, _ = sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})

	// MarkSubmitted from approved (skipping executing) → not allowed.
	// This is the safety guarantee: approved → submitted directly would
	// hide the crash window during which Submit() was running.
	if err := sm.MarkSubmitted(context.Background(), ps.ID, 42); !errors.Is(err, ErrNotPending) {
		t.Fatalf("MarkSubmitted from approved (skipping executing): want ErrNotPending, got %v", err)
	}

	if err := sm.MarkExecuting(context.Background(), ps.ID); err != nil {
		t.Fatalf("MarkExecuting: %v", err)
	}
	if err := sm.MarkSubmitted(context.Background(), ps.ID, 42); err != nil {
		t.Fatalf("MarkSubmitted from executing: %v", err)
	}

	// Idempotent re-call from submitted state → still ErrNotPending.
	if err := sm.MarkSubmitted(context.Background(), ps.ID, 42); !errors.Is(err, ErrNotPending) {
		t.Fatalf("second MarkSubmitted: want ErrNotPending, got %v", err)
	}
}

func TestMarkFailedAcceptsApprovedOrExecuting(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})

	a, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Approve(context.Background(), a.ID, Decision{By: "op", Source: "api"})
	if err := sm.MarkFailed(context.Background(), a.ID, "claim failed"); err != nil {
		t.Fatalf("MarkFailed from approved: %v", err)
	}

	b, _ := sm.Enqueue(context.Background(), sampleInput(2))
	_, _ = sm.Approve(context.Background(), b.ID, Decision{By: "op", Source: "api"})
	_ = sm.MarkExecuting(context.Background(), b.ID)
	if err := sm.MarkFailed(context.Background(), b.ID, "binance 500"); err != nil {
		t.Fatalf("MarkFailed from executing: %v", err)
	}

	if err := sm.MarkFailed(context.Background(), b.ID, "again"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("MarkFailed on terminal: want ErrNotPending, got %v", err)
	}
}

func TestConcurrentMarkExecutingSingleWinner(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})

	const N = 10
	var ok atomic.Int32
	var notPending atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := sm.MarkExecuting(context.Background(), ps.ID)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrNotPending):
				notPending.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 {
		t.Fatalf("expected exactly 1 winner, got %d (notPending=%d)", ok.Load(), notPending.Load())
	}
}

// TestConcurrentApproveSingleWinner is the heart of "standard, not bandaid":
// N concurrent Approve calls against the same pending row must yield
// exactly one success. SQLite's serialised writer simulates the Postgres
// atomic-update guarantee.
func TestConcurrentApproveSingleWinner(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))

	const N = 10
	var success atomic.Int32
	var notPending atomic.Int32
	var other atomic.Int32

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
			switch {
			case err == nil:
				success.Add(1)
			case errors.Is(err, ErrNotPending):
				notPending.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if success.Load() != 1 {
		t.Fatalf("expected exactly 1 successful approve, got %d (notPending=%d other=%d)",
			success.Load(), notPending.Load(), other.Load())
	}
	if other.Load() != 0 {
		t.Fatalf("unexpected non-ErrNotPending failures: %d", other.Load())
	}
}

func TestListPendingReturnsOnlyPending(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 10})

	a, _ := sm.Enqueue(context.Background(), sampleInput(1))
	b, _ := sm.Enqueue(context.Background(), sampleInput(2))
	c, _ := sm.Enqueue(context.Background(), sampleInput(3))
	_, _ = sm.Approve(context.Background(), b.ID, Decision{By: "op", Source: "api"})
	_, _ = sm.Reject(context.Background(), c.ID, "r", Decision{By: "op", Source: "api"})

	list, err := sm.ListPending(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].ID != a.ID {
		t.Fatalf("expected only [%d] pending, got %v", a.ID, idsOf(list))
	}
}

func idsOf(rows []models.PendingSignal) []uint {
	out := make([]uint, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
