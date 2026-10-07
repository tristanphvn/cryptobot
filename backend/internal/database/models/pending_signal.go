package models

import (
	"time"

	"github.com/shopspring/decimal"
)

// PendingSignalStatus enumerates the terminal and non-terminal states of
// the human-in-the-loop approval gate. The state machine and its allowed
// transitions are documented in package approval.
// PendingSignalStatus enumerates every node of the approval state
// machine. The lifecycle is:
//
//	   PENDING
//	   ├── APPROVED ── EXECUTING ── SUBMITTED   (terminal)
//	   │                       └── FAILED      (terminal — risk/exchange/etc.)
//	   ├── REJECTED                            (terminal — user)
//	   └── EXPIRED                             (terminal — sweeper TTL)
//
// EXECUTING is the intermediate "we have started work but it has not
// finalised" state. It exists so that a crash between Submit() and
// MarkSubmitted() leaves a row in a recoverable state instead of
// silently stuck at APPROVED. The sweeper marks executing rows older
// than the recovery window as FAILED with reason="crash recovery —
// reconcile via exchange" so the operator can verify out-of-band.
type PendingSignalStatus string

const (
	PendingSignalPending   PendingSignalStatus = "pending"   // awaiting user decision
	PendingSignalApproved  PendingSignalStatus = "approved"  // user approved; executor about to submit
	PendingSignalExecuting PendingSignalStatus = "executing" // submission in flight (exchange call running)
	PendingSignalRejected  PendingSignalStatus = "rejected"  // user rejected (terminal)
	PendingSignalExpired   PendingSignalStatus = "expired"   // sweeper expired (terminal)
	PendingSignalSubmitted PendingSignalStatus = "submitted" // execution accepted (terminal)
	PendingSignalFailed    PendingSignalStatus = "failed"    // execution rejected or recovered-as-unknown
)

// PendingSignal is the durable state record for one AI signal awaiting
// (or having received) a human decision before order submission. Only
// active when EXECUTION_MODE=live; dry-run and paper modes bypass this
// table entirely and auto-execute as before.
//
// The state machine is atomic via UPDATE ... WHERE status='pending' AND
// expires_at > NOW(); see internal/approval/state.go for transitions.
//
// UNIQUE(audit_log_id) guarantees idempotent Enqueue: a duplicate insert
// (e.g. executor crash + retry mid-publish) is silently dropped, so the
// pending queue cannot accumulate phantom rows.
type PendingSignal struct {
	ID uint `gorm:"primaryKey" json:"id"`

	// Identity of the underlying AI evaluation
	Symbol     string `gorm:"type:varchar(20);not null;index" json:"symbol"`
	Interval   string `gorm:"type:varchar(5);not null"        json:"interval"`
	AuditLogID uint   `gorm:"uniqueIndex;not null"            json:"audit_log_id"`

	// Snapshot of the proposed action (the audit log holds the full prompt
	// and reasoning; we duplicate the essentials here so /signals/pending
	// is a single-query call without joins).
	Action      string          `gorm:"type:varchar(10);not null"      json:"action"`
	Confidence  float64         `gorm:"type:numeric(5,4);not null"     json:"confidence"`
	Reasoning   string          `gorm:"type:text"                      json:"reasoning"`
	ProposedSL  decimal.Decimal `gorm:"type:numeric(20,8)"             json:"proposed_sl"`
	ProposedTP  decimal.Decimal `gorm:"type:numeric(20,8)"             json:"proposed_tp"`
	MarketPrice decimal.Decimal `gorm:"type:numeric(20,8);not null"    json:"market_price"`

	// State machine
	Status         PendingSignalStatus `gorm:"type:varchar(12);not null;default:'pending';index" json:"status"`
	RejectedReason string              `gorm:"type:text"                                          json:"rejected_reason,omitempty"`
	DecidedAt      *time.Time          `json:"decided_at,omitempty"`
	ExpiresAt      time.Time           `gorm:"not null;index"                                     json:"expires_at"`
	ExecutingAt    *time.Time          `json:"executing_at,omitempty"` // set when CAS approved→executing; used by sweeper recovery
	TradeID        *uint               `gorm:"index"                                              json:"trade_id,omitempty"`

	// Approval provenance (forensics). Required even for single-operator
	// mode so audits can attribute decisions to a specific session, and
	// so a future multi-operator setup has the foundation already.
	DecidedBy             string `gorm:"type:varchar(50)"                              json:"decided_by,omitempty"`              // operator id / "system:sweeper"
	ApprovalSource        string `gorm:"type:varchar(20)"                              json:"approval_source,omitempty"`         // "manual" | "api" | "system"
	ApprovalIP            string `gorm:"type:varchar(45)"                              json:"approval_ip,omitempty"`             // IPv4 or IPv6
	ApprovalUserAgent     string `gorm:"type:varchar(255)"                             json:"approval_user_agent,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (PendingSignal) TableName() string { return "pending_signals" }
