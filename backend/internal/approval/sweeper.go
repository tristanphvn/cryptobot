package approval

import (
	"context"
	"log"
	"time"

	"cryptobot/internal/database/models"

	"gorm.io/gorm"
)

// Sweeper expires pending signals past their TTL AND recovers stuck
// executing rows after a crash. Both passes run on startup before the
// ticker so the system converges to a clean state immediately.
//
// Standard, not a bandaid: expiration is server-authoritative. The UI
// countdown is cosmetic — only this sweeper (or an explicit user edge)
// can move a row out of pending. Likewise, a process restart leaves
// executing rows orphaned without external evidence. CRY-15 wires a
// Reconciler that queries the exchange by newClientOrderId before
// touching the row; only when the reconciler is absent (or returns
// ambiguous) does the sweeper fall back to the legacy MarkFailed pass.
type Sweeper struct {
	db              *gorm.DB
	interval        time.Duration
	executingMaxAge time.Duration              // executing > this → recovered as FAILED
	onExpired       func(models.PendingSignal) // optional WS emitter for expired
	onRecovered     func(models.PendingSignal) // optional WS emitter for crash-recovered
	reconciler      Reconciler                 // CRY-15: queries the exchange before MarkFailed
}

// Reconciler resolves a stuck `executing` pending_signal by querying the
// venue with newClientOrderId="cry5-<ps.ID>" and transitioning the row
// to the correct terminal state (submitted or failed). Returning a
// non-nil error means the venue itself was ambiguous (network err / 5xx
// / non-terminal status); the sweeper leaves the row in executing and
// tries again on the next tick. Returning nil means the row has been
// transitioned (or in the not-found case, marked failed) and the
// sweeper should NOT run its fallback MarkFailed pass for it.
type Reconciler interface {
	Reconcile(ctx context.Context, ps models.PendingSignal) error
}

// NewSweeper wires the sweeper. interval defaults to 5s; executingMaxAge
// defaults to 60s (an executing row older than that almost certainly
// reflects a crashed worker — Submit calls are sub-second in v1; CRY-15
// may extend this when Binance polling enters the picture).
func NewSweeper(db *gorm.DB, interval, executingMaxAge time.Duration) *Sweeper {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if executingMaxAge <= 0 {
		executingMaxAge = 60 * time.Second
	}
	return &Sweeper{db: db, interval: interval, executingMaxAge: executingMaxAge}
}

// OnExpired registers a callback fired for each pending signal that
// gets swept to expired.
func (s *Sweeper) OnExpired(fn func(models.PendingSignal)) { s.onExpired = fn }

// OnRecovered registers a callback fired for each executing row that
// the sweeper marks as FAILED via crash recovery.
func (s *Sweeper) OnRecovered(fn func(models.PendingSignal)) { s.onRecovered = fn }

// WithReconciler wires a Reconciler that the sweeper consults before
// the fallback MarkFailed pass. When set, the legacy "reconcile via
// exchange" reason is suppressed in favour of the reconciler's own
// resolution (it can MarkSubmitted with a real trade_id when Binance
// reports FILLED, or MarkFailed with a precise reason).
func (s *Sweeper) WithReconciler(r Reconciler) *Sweeper {
	s.reconciler = r
	return s
}

// Run blocks until ctx is cancelled. Runs an immediate sweep on entry
// (the startup recovery) before entering the ticker loop. Designed to
// run under supervise() from main.go.
func (s *Sweeper) Run(ctx context.Context) {
	log.Printf("[approval-sweeper] started — interval=%s exec_max_age=%s",
		s.interval, s.executingMaxAge)
	s.sweepExpired(ctx, "startup")
	s.sweepCrashRecovery(ctx, "startup")

	t := time.NewTicker(s.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("[approval-sweeper] stopped")
			return
		case <-t.C:
			s.sweepExpired(ctx, "tick")
			s.sweepCrashRecovery(ctx, "tick")
		}
	}
}

// sweepExpired is one pass of "find pending+past-TTL, mark expired,
// fire callbacks". Snapshot-then-update avoids racing with a concurrent
// Approve: any row that flips to approved between the SELECT and the
// UPDATE is naturally skipped by the UPDATE's WHERE status='pending'
// guard.
func (s *Sweeper) sweepExpired(ctx context.Context, reason string) {
	now := time.Now()

	var expiring []models.PendingSignal
	if err := s.db.WithContext(ctx).
		Where("status = ? AND expires_at <= ?", models.PendingSignalPending, now).
		Find(&expiring).Error; err != nil {
		log.Printf("[approval-sweeper] expired select err: %v", err)
		return
	}
	if len(expiring) == 0 {
		return
	}

	res := s.db.WithContext(ctx).
		Model(&models.PendingSignal{}).
		Where("status = ? AND expires_at <= ?", models.PendingSignalPending, now).
		Updates(map[string]any{
			"status":          models.PendingSignalExpired,
			"decided_at":      now,
			"decided_by":      "system:sweeper",
			"approval_source": "system",
			"rejected_reason": "TTL expired",
		})
	if res.Error != nil {
		log.Printf("[approval-sweeper] expired update err: %v", res.Error)
		return
	}
	log.Printf("[approval-sweeper] reason=%s expired=%d", reason, res.RowsAffected)

	if s.onExpired != nil {
		for i := range expiring {
			expiring[i].Status = models.PendingSignalExpired
			s.onExpired(expiring[i])
		}
	}
}

// sweepCrashRecovery handles executing rows older than executingMaxAge.
// When a Reconciler is wired (CRY-15), each row is resolved by querying
// the exchange — Binance is the source of truth, so a FILLED outcome
// becomes `submitted` with a real trade_id and a CANCELED/EXPIRED/
// rejected outcome becomes `failed` with a precise reason. Only when no
// reconciler is configured does the sweeper fall back to the legacy
// MarkFailed-with-vague-reason behaviour so the operator still has a
// visible queue to investigate.
func (s *Sweeper) sweepCrashRecovery(ctx context.Context, reason string) {
	cutoff := time.Now().Add(-s.executingMaxAge)

	var stuck []models.PendingSignal
	if err := s.db.WithContext(ctx).
		Where("status = ? AND executing_at <= ?", models.PendingSignalExecuting, cutoff).
		Find(&stuck).Error; err != nil {
		log.Printf("[approval-sweeper] recovery select err: %v", err)
		return
	}
	if len(stuck) == 0 {
		return
	}

	if s.reconciler != nil {
		s.reconcileStuck(ctx, reason, stuck)
		return
	}
	s.markStuckFailed(ctx, reason, stuck)
}

// reconcileStuck delegates each stuck row to the configured Reconciler.
// On an ambiguous response (non-nil err) the row is left in executing
// so the next tick can retry; on a clean resolution we re-read the row
// to fire OnRecovered with its new terminal state.
func (s *Sweeper) reconcileStuck(ctx context.Context, reason string, stuck []models.PendingSignal) {
	resolved := 0
	for _, ps := range stuck {
		if err := s.reconciler.Reconcile(ctx, ps); err != nil {
			log.Printf("[approval-sweeper] reconcile id=%d err=%v — leaving in executing", ps.ID, err)
			continue
		}
		resolved++
		if s.onRecovered != nil {
			var updated models.PendingSignal
			if err := s.db.WithContext(ctx).First(&updated, ps.ID).Error; err == nil {
				s.onRecovered(updated)
			}
		}
	}
	if resolved > 0 {
		log.Printf("[approval-sweeper] reason=%s reconciled=%d/%d (rows older than %s)",
			reason, resolved, len(stuck), s.executingMaxAge)
	}
}

// markStuckFailed is the legacy "no reconciler — mark and surface to
// operator" path. Retained so non-live deployments (no provider) still
// have a graceful crash recovery story.
func (s *Sweeper) markStuckFailed(ctx context.Context, reason string, stuck []models.PendingSignal) {
	cutoff := time.Now().Add(-s.executingMaxAge)
	res := s.db.WithContext(ctx).
		Model(&models.PendingSignal{}).
		Where("status = ? AND executing_at <= ?", models.PendingSignalExecuting, cutoff).
		Updates(map[string]any{
			"status":          models.PendingSignalFailed,
			"rejected_reason": "crash recovery — reconcile via exchange",
		})
	if res.Error != nil {
		log.Printf("[approval-sweeper] recovery update err: %v", res.Error)
		return
	}
	log.Printf("[approval-sweeper] reason=%s crash_recovered=%d (rows older than %s) — OPERATOR ACTION REQUIRED",
		reason, res.RowsAffected, s.executingMaxAge)

	if s.onRecovered != nil {
		for i := range stuck {
			stuck[i].Status = models.PendingSignalFailed
			s.onRecovered(stuck[i])
		}
	}
}
