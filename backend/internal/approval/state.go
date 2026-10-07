// Package approval implements the human-in-the-loop state machine that
// sits between AI signal generation and order submission when
// EXECUTION_MODE=live. Dry-run and paper modes do not interact with this
// package — the executor auto-executes as before.
//
// # Must be a standard, not a bandaid
//
// Every transition is a single atomic UPDATE guarded on the current
// status + expires_at, so concurrent approve/reject/expire races resolve
// to exactly one winner. Idempotency is guaranteed in three places:
//
//   - Enqueue is idempotent on audit_log_id (UNIQUE constraint + ON
//     CONFLICT DO NOTHING).
//   - Approve / Reject are idempotent because the WHERE clause requires
//     status='pending'; a second call observes RowsAffected=0.
//   - MarkSubmitted / MarkFailed are idempotent because they require
//     status='approved'.
//
// This package owns no goroutines and has no global state beyond the
// gorm DB handle, so two instances against the same DB share state
// correctly. A horizontally-scaled executor could plug in directly
// (subject to additional advisory-lock work documented in CRY-5).
package approval

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"cryptobot/internal/database/models"

	"github.com/shopspring/decimal"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Errors returned by the state machine. Distinct sentinel values so HTTP
// handlers can map to status codes without string-matching.
var (
	ErrQueueFull  = errors.New("approval queue is full")
	ErrNotPending = errors.New("pending signal is not in pending state")
	ErrExpired    = errors.New("pending signal has expired")
	ErrNotFound   = errors.New("pending signal not found")
)

// Config carries operator-tunable parameters. Loaded from env at startup.
type Config struct {
	TTL      time.Duration // how long a pending signal stays open for review
	QueueMax int           // hard cap on concurrent pending signals
}

func DefaultConfig() Config {
	return Config{TTL: 60 * time.Second, QueueMax: 10}
}

// StateMachine owns the durable approval lifecycle. Construct one per
// process; safe for concurrent use from any number of goroutines.
type StateMachine struct {
	db  *gorm.DB
	cfg Config
}

func NewStateMachine(db *gorm.DB, cfg Config) *StateMachine {
	if cfg.TTL <= 0 {
		cfg.TTL = 60 * time.Second
	}
	if cfg.QueueMax <= 0 {
		cfg.QueueMax = 10
	}
	return &StateMachine{db: db, cfg: cfg}
}

// EnqueueInput is the input shape for new signals awaiting approval.
type EnqueueInput struct {
	Symbol      string
	Interval    string
	AuditLogID  uint
	Action      string
	Confidence  float64
	Reasoning   string
	ProposedSL  decimal.Decimal
	ProposedTP  decimal.Decimal
	MarketPrice decimal.Decimal
}

// Enqueue creates a new pending_signal row, enforcing QueueMax.
//
// Returns ErrQueueFull when at capacity — callers must record this in
// audit_logs (action=BLOCKED, blocked_by=queue_full) so the operator can
// see that AI output was suppressed.
//
// Idempotent on AuditLogID: a duplicate (e.g. executor crash + retry)
// returns the existing row instead of creating a second one.
func (sm *StateMachine) Enqueue(ctx context.Context, in EnqueueInput) (*models.PendingSignal, error) {
	var count int64
	if err := sm.db.WithContext(ctx).
		Model(&models.PendingSignal{}).
		Where("status = ?", models.PendingSignalPending).
		Count(&count).Error; err != nil {
		return nil, fmt.Errorf("count pending: %w", err)
	}
	if int(count) >= sm.cfg.QueueMax {
		return nil, ErrQueueFull
	}

	ps := models.PendingSignal{
		Symbol:      in.Symbol,
		Interval:    in.Interval,
		AuditLogID:  in.AuditLogID,
		Action:      in.Action,
		Confidence:  in.Confidence,
		Reasoning:   in.Reasoning,
		ProposedSL:  in.ProposedSL,
		ProposedTP:  in.ProposedTP,
		MarketPrice: in.MarketPrice,
		Status:      models.PendingSignalPending,
		ExpiresAt:   time.Now().Add(sm.cfg.TTL),
	}

	err := sm.db.WithContext(ctx).
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "audit_log_id"}},
			DoNothing: true,
		}).
		Create(&ps).Error
	if err != nil {
		return nil, fmt.Errorf("insert: %w", err)
	}
	if ps.ID == 0 {
		// ON CONFLICT path — fetch the existing row.
		if err := sm.db.WithContext(ctx).
			Where("audit_log_id = ?", in.AuditLogID).
			First(&ps).Error; err != nil {
			return nil, fmt.Errorf("fetch on conflict: %w", err)
		}
	} else {
		log.Printf("[approval] enqueued id=%d sym=%s action=%s expires_in=%s",
			ps.ID, ps.Symbol, ps.Action, time.Until(ps.ExpiresAt).Round(time.Second))
	}
	return &ps, nil
}

// Decision carries the forensics provenance of a user-initiated edge.
// Stored on the row so audits can reconstruct who approved/rejected and
// from where, even for single-operator mode.
type Decision struct {
	By        string // operator identifier (from auth middleware)
	Source    string // "manual" | "api" | "system"
	IP        string // remote addr
	UserAgent string // browser / curl version etc.
}

// Approve atomically transitions a pending signal to approved. Guards:
//   - row exists (ErrNotFound)
//   - status == pending (ErrNotPending)
//   - expires_at > NOW() (ErrExpired)
//
// Provenance fields are persisted on the row so the audit log can
// answer "who approved this trade, from where, with what tool".
func (sm *StateMachine) Approve(ctx context.Context, id uint, d Decision) (*models.PendingSignal, error) {
	return sm.transition(ctx, id, models.PendingSignalApproved, "", d)
}

// Reject atomically transitions a pending signal to rejected. Same
// guards as Approve. `reason` is required by convention for the audit
// trail, but the state machine itself does not enforce non-empty.
func (sm *StateMachine) Reject(ctx context.Context, id uint, reason string, d Decision) (*models.PendingSignal, error) {
	return sm.transition(ctx, id, models.PendingSignalRejected, reason, d)
}

// transition is the single UPDATE WHERE guard for user-initiated edges.
// Disambiguates RowsAffected=0 into NotFound vs NotPending vs Expired so
// the HTTP layer can map to clear 4xx codes.
func (sm *StateMachine) transition(
	ctx context.Context,
	id uint,
	target models.PendingSignalStatus,
	reason string,
	d Decision,
) (*models.PendingSignal, error) {
	now := time.Now()
	updates := map[string]any{
		"status":              target,
		"decided_at":          now,
		"decided_by":          d.By,
		"approval_source":     d.Source,
		"approval_ip":         d.IP,
		"approval_user_agent": d.UserAgent,
	}
	if target == models.PendingSignalRejected {
		updates["rejected_reason"] = reason
	}

	res := sm.db.WithContext(ctx).
		Model(&models.PendingSignal{}).
		Where("id = ? AND status = ? AND expires_at > ?", id, models.PendingSignalPending, now).
		Updates(updates)
	if res.Error != nil {
		return nil, fmt.Errorf("transition: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		var ps models.PendingSignal
		if err := sm.db.WithContext(ctx).Where("id = ?", id).First(&ps).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("disambiguate: %w", err)
		}
		if ps.Status != models.PendingSignalPending {
			return nil, ErrNotPending
		}
		return nil, ErrExpired
	}
	var updated models.PendingSignal
	if err := sm.db.WithContext(ctx).Where("id = ?", id).First(&updated).Error; err != nil {
		return nil, err
	}
	log.Printf("[approval] id=%d sym=%s action=%s → %s (by=%s src=%s ip=%s)",
		updated.ID, updated.Symbol, updated.Action, target, d.By, d.Source, d.IP)
	return &updated, nil
}

// MarkExecuting atomically claims an approved row for submission work.
// The handler calls this BEFORE invoking the submitter, so a process
// crash mid-submission leaves the row in `executing` (recoverable by
// the sweeper) instead of `approved` (silently lost).
//
// Idempotent: a retry on the same row observes RowsAffected=0 and
// returns ErrNotPending so the caller knows another worker (or its
// previous self) has already claimed it.
func (sm *StateMachine) MarkExecuting(ctx context.Context, id uint) error {
	now := time.Now()
	res := sm.db.WithContext(ctx).
		Model(&models.PendingSignal{}).
		Where("id = ? AND status = ?", id, models.PendingSignalApproved).
		Updates(map[string]any{
			"status":       models.PendingSignalExecuting,
			"executing_at": now,
		})
	if res.Error != nil {
		return fmt.Errorf("mark executing: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotPending
	}
	log.Printf("[approval] id=%d → executing", id)
	return nil
}

// MarkSubmitted records that execution succeeded for an executing row.
// The row MUST be in `executing` (claimed via MarkExecuting) — a direct
// approved → submitted jump is intentionally not allowed because it
// would hide the crash window during which Submit() was running.
func (sm *StateMachine) MarkSubmitted(ctx context.Context, id, tradeID uint) error {
	res := sm.db.WithContext(ctx).
		Model(&models.PendingSignal{}).
		Where("id = ? AND status = ?", id, models.PendingSignalExecuting).
		Updates(map[string]any{
			"status":   models.PendingSignalSubmitted,
			"trade_id": tradeID,
		})
	if res.Error != nil {
		return fmt.Errorf("mark submitted: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotPending
	}
	log.Printf("[approval] id=%d → submitted trade_id=%d", id, tradeID)
	return nil
}

// MarkFailed records that execution failed. Accepts rows in either
// `approved` (failure during MarkExecuting itself) or `executing`
// (failure during the submission work). Idempotent on terminal states.
func (sm *StateMachine) MarkFailed(ctx context.Context, id uint, reason string) error {
	res := sm.db.WithContext(ctx).
		Model(&models.PendingSignal{}).
		Where("id = ? AND status IN ?", id, []models.PendingSignalStatus{
			models.PendingSignalApproved,
			models.PendingSignalExecuting,
		}).
		Updates(map[string]any{
			"status":          models.PendingSignalFailed,
			"rejected_reason": reason,
		})
	if res.Error != nil {
		return fmt.Errorf("mark failed: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrNotPending
	}
	log.Printf("[approval] id=%d → failed reason=%s", id, reason)
	return nil
}

// ListPending returns the currently-pending signals, oldest first. Used
// by GET /signals/pending and by the WS-reconnect re-hydration path.
func (sm *StateMachine) ListPending(ctx context.Context) ([]models.PendingSignal, error) {
	var rows []models.PendingSignal
	err := sm.db.WithContext(ctx).
		Where("status = ?", models.PendingSignalPending).
		Order("created_at ASC").
		Find(&rows).Error
	return rows, err
}

// GetByID returns a single pending signal regardless of status. Used by
// the HTTP layer for 404 vs 409 disambiguation in error responses.
func (sm *StateMachine) GetByID(ctx context.Context, id uint) (*models.PendingSignal, error) {
	var ps models.PendingSignal
	err := sm.db.WithContext(ctx).Where("id = ?", id).First(&ps).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &ps, nil
}
