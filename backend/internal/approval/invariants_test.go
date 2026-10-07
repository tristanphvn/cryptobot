package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"cryptobot/internal/database/models"
)

// invariants_test.go exists for one purpose: prove every forbidden
// transition fails deterministically with an auditable error.
//
// Allowed edges:
//   PENDING   → APPROVED        (Approve)
//   PENDING   → REJECTED        (Reject)
//   PENDING   → EXPIRED         (sweeper)
//   APPROVED  → EXECUTING       (MarkExecuting)
//   EXECUTING → SUBMITTED       (MarkSubmitted) — "EXECUTED" in the graph
//   APPROVED  → FAILED          (MarkFailed — risk gate failure path)
//   EXECUTING → FAILED          (MarkFailed — submission failure path)
//
// Forbidden edges (this file enforces all of them via direct tests):
//   APPROVED  → SUBMITTED directly    (skipping executing hides crash window)
//   PENDING   → EXECUTING             (must approve first)
//   SUBMITTED → FAILED                (terminal state)
//   FAILED    → EXECUTING             (terminal state)
//   EXPIRED   → APPROVED              (terminal state)
//   REJECTED  → APPROVED              (terminal state)

func TestForbiddenEdge_ApprovedToSubmittedDirectly(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})

	if err := sm.MarkSubmitted(context.Background(), ps.ID, 42); !errors.Is(err, ErrNotPending) {
		t.Fatalf("approved→submitted must fail; got %v", err)
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalApproved {
		t.Fatalf("status changed despite forbidden edge: %s", got.Status)
	}
}

func TestForbiddenEdge_PendingToExecuting(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))

	if err := sm.MarkExecuting(context.Background(), ps.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("pending→executing must fail; got %v", err)
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalPending {
		t.Fatalf("status changed despite forbidden edge: %s", got.Status)
	}
}

func TestForbiddenEdge_SubmittedToFailed(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	_ = sm.MarkExecuting(context.Background(), ps.ID)
	_ = sm.MarkSubmitted(context.Background(), ps.ID, 99)

	if err := sm.MarkFailed(context.Background(), ps.ID, "should not transition"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("submitted→failed must fail; got %v", err)
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalSubmitted {
		t.Fatalf("status changed despite forbidden edge: %s", got.Status)
	}
}

func TestForbiddenEdge_FailedToExecuting(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	_ = sm.MarkFailed(context.Background(), ps.ID, "first")

	if err := sm.MarkExecuting(context.Background(), ps.ID); !errors.Is(err, ErrNotPending) {
		t.Fatalf("failed→executing must fail; got %v", err)
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalFailed {
		t.Fatalf("status changed despite forbidden edge: %s", got.Status)
	}
}

func TestForbiddenEdge_ExpiredToApproved(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	// Force the row to expired status manually (as if sweeper had run).
	db.Model(&models.PendingSignal{}).Where("id = ?", ps.ID).
		Update("status", models.PendingSignalExpired)

	_, err := sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("expired→approved must fail; got %v", err)
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalExpired {
		t.Fatalf("status changed despite forbidden edge: %s", got.Status)
	}
}

func TestForbiddenEdge_RejectedToApproved(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Reject(context.Background(), ps.ID, "no", Decision{By: "op", Source: "api"})

	_, err := sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("rejected→approved must fail; got %v", err)
	}
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalRejected {
		t.Fatalf("status changed despite forbidden edge: %s", got.Status)
	}
}

// TestConcurrentApproveAndReject — heart of the multi-tab safety claim:
// one operator approves on tab A while another (or the same) rejects on
// tab B. The DB serialises winners; exactly one transition succeeds.
func TestConcurrentApproveAndReject(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))

	var approveOk, rejectOk bool
	var approveErr, rejectErr error
	done := make(chan struct{}, 2)

	go func() {
		_, err := sm.Approve(context.Background(), ps.ID, Decision{By: "tab-a", Source: "api"})
		if err == nil {
			approveOk = true
		} else {
			approveErr = err
		}
		done <- struct{}{}
	}()
	go func() {
		_, err := sm.Reject(context.Background(), ps.ID, "no", Decision{By: "tab-b", Source: "api"})
		if err == nil {
			rejectOk = true
		} else {
			rejectErr = err
		}
		done <- struct{}{}
	}()
	<-done
	<-done

	if approveOk == rejectOk {
		t.Fatalf("expected exactly one winner — approveOk=%v rejectOk=%v approveErr=%v rejectErr=%v",
			approveOk, rejectOk, approveErr, rejectErr)
	}
}
