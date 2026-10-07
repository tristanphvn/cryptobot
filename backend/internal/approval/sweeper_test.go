package approval

import (
	"context"
	"errors"
	"testing"
	"time"

	"cryptobot/internal/database/models"

	"gorm.io/gorm"
)

func TestSweeperExpiresPastTTLRows(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))

	// Back-date so the row is expired by now.
	db.Model(&models.PendingSignal{}).Where("id = ?", ps.ID).Update("expires_at", time.Now().Add(-time.Second))

	var expired []models.PendingSignal
	sw := NewSweeper(db, time.Hour, time.Minute)
	sw.OnExpired(func(p models.PendingSignal) { expired = append(expired, p) })
	sw.sweepExpired(context.Background(), "test")

	if len(expired) != 1 || expired[0].ID != ps.ID {
		t.Fatalf("expected one expired callback for id=%d, got %v", ps.ID, expired)
	}

	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalExpired {
		t.Fatalf("status=%s want expired", got.Status)
	}
	if got.RejectedReason == "" {
		t.Fatal("rejected_reason should be populated on auto-expire")
	}
}

// TestSweeperCrashRecoveryTransitionsStuckExecutingToFailed exercises
// the killer scenario for "standard, not a bandaid": a process crash
// after MarkExecuting + before MarkSubmitted leaves a row in executing.
// The sweeper must move it to failed with a clear reason so the
// operator can reconcile against the exchange.
func TestSweeperCrashRecoveryTransitionsStuckExecutingToFailed(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	if err := sm.MarkExecuting(context.Background(), ps.ID); err != nil {
		t.Fatalf("MarkExecuting: %v", err)
	}

	// Simulate a crash 10 minutes ago — back-date executing_at past the
	// recovery window.
	db.Model(&models.PendingSignal{}).
		Where("id = ?", ps.ID).
		Update("executing_at", time.Now().Add(-10*time.Minute))

	var recovered []models.PendingSignal
	// Recovery window = 60s; any executing row older than that gets recovered.
	sw := NewSweeper(db, time.Hour, 60*time.Second)
	sw.OnRecovered(func(p models.PendingSignal) { recovered = append(recovered, p) })
	sw.sweepCrashRecovery(context.Background(), "test")

	if len(recovered) != 1 || recovered[0].ID != ps.ID {
		t.Fatalf("expected one recovery callback for id=%d, got %v", ps.ID, recovered)
	}

	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalFailed {
		t.Fatalf("status=%s want failed", got.Status)
	}
	if got.RejectedReason == "" {
		t.Fatal("rejected_reason should be populated on crash recovery — operator needs the hint")
	}
}

// fakeReconciler lets tests assert how the sweeper interacts with a
// configured Reconciler. CRY-15: when present, the sweeper MUST defer
// to the reconciler instead of running its blanket MarkFailed pass.
type fakeReconciler struct {
	calls       []uint
	resolveErr  error
	transition  func(db *gorm.DB, ps models.PendingSignal) // optional side effect on success
}

func (f *fakeReconciler) Reconcile(_ context.Context, ps models.PendingSignal) error {
	f.calls = append(f.calls, ps.ID)
	return f.resolveErr
}

func TestSweeperWithReconciler_DefersToIt(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	_ = sm.MarkExecuting(context.Background(), ps.ID)
	// Back-date so the row is past the recovery window.
	db.Model(&models.PendingSignal{}).Where("id = ?", ps.ID).
		Update("executing_at", time.Now().Add(-10*time.Minute))

	rec := &fakeReconciler{}
	sw := NewSweeper(db, time.Hour, 60*time.Second).WithReconciler(rec)
	sw.sweepCrashRecovery(context.Background(), "test")

	if len(rec.calls) != 1 || rec.calls[0] != ps.ID {
		t.Fatalf("reconciler calls=%v, want [%d]", rec.calls, ps.ID)
	}
	// Row must NOT be auto-marked failed when a reconciler is configured —
	// the reconciler owns the transition. With a fake that doesn't move
	// the row, it stays in executing.
	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalExecuting {
		t.Fatalf("status=%s want executing (reconciler stub didn't move it; sweeper must not fall back to MarkFailed)", got.Status)
	}
}

func TestSweeperWithReconciler_AmbiguousErrLeavesRow(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	_ = sm.MarkExecuting(context.Background(), ps.ID)
	db.Model(&models.PendingSignal{}).Where("id = ?", ps.ID).
		Update("executing_at", time.Now().Add(-10*time.Minute))

	rec := &fakeReconciler{resolveErr: errors.New("network down")}
	sw := NewSweeper(db, time.Hour, 60*time.Second).WithReconciler(rec)
	sw.sweepCrashRecovery(context.Background(), "test")

	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalExecuting {
		t.Fatalf("status=%s want executing — ambiguous reconcile must leave row alone for next tick", got.Status)
	}
}

func TestSweeperLeavesFreshExecutingAlone(t *testing.T) {
	db := newTestDB(t)
	sm := NewStateMachine(db, Config{TTL: 30 * time.Second, QueueMax: 5})
	ps, _ := sm.Enqueue(context.Background(), sampleInput(1))
	_, _ = sm.Approve(context.Background(), ps.ID, Decision{By: "op", Source: "api"})
	_ = sm.MarkExecuting(context.Background(), ps.ID)

	// Generous recovery window — the row should NOT be touched.
	sw := NewSweeper(db, time.Hour, time.Hour)
	sw.sweepCrashRecovery(context.Background(), "test")

	var got models.PendingSignal
	db.First(&got, ps.ID)
	if got.Status != models.PendingSignalExecuting {
		t.Fatalf("status=%s want executing (fresh row should not be recovered)", got.Status)
	}
}
