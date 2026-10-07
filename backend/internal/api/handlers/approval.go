package handlers

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"cryptobot/internal/approval"

	"github.com/gin-gonic/gin"
)

// Submitter is the executor-side contract for handing an approved signal
// off to order placement. Kept tiny so the handler does not depend on
// the full executor package — easier to test, easier to swap when
// CRY-15 wires real Binance order placement.
type Submitter interface {
	Submit(ctx *gin.Context, pendingID uint) (tradeID uint, err error)
}

// Notifier is the WS-broadcast contract. *Hub satisfies it; tests
// inject a fake recorder. Keeping this an interface (not a *Hub
// dependency) keeps the package boundary clean and the unit tests
// trivially constructable.
type Notifier interface {
	Broadcast(map[string]any)
}

// ApprovalHandler exposes the human-in-the-loop endpoints. All three
// routes must sit behind middleware.Bearer in router wiring; this
// handler does no auth of its own.
type ApprovalHandler struct {
	sm        *approval.StateMachine
	submitter Submitter
	notifier  Notifier
}

// NewApprovalHandler wires the handler with optional notifier. notifier
// may be nil in tests; production passes the WS hub so user decisions
// broadcast immediately to every connected dashboard.
func NewApprovalHandler(sm *approval.StateMachine, submitter Submitter, notifier Notifier) *ApprovalHandler {
	return &ApprovalHandler{sm: sm, submitter: submitter, notifier: notifier}
}

// broadcast is a nil-safe wrapper so production and test paths are the
// same code at every call-site.
func (h *ApprovalHandler) broadcast(msgType string, payload any) {
	if h.notifier == nil {
		return
	}
	h.notifier.Broadcast(map[string]any{"type": msgType, "data": payload})
}

// ListPending answers GET /api/v1/signals/pending. The UI re-hydrates
// from this on connect / WS reconnect so the realtime stream only needs
// to deliver diffs.
func (h *ApprovalHandler) ListPending(c *gin.Context) {
	rows, err := h.sm.ListPending(c.Request.Context())
	if err != nil {
		log.Printf("[approval] list err: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"pending": rows})
}

// rejectBody is the optional body for POST /signals/:id/reject. Reason
// is recorded in audit_logs; missing → "user-rejected".
type rejectBody struct {
	Reason string `json:"reason"`
}

// Approve handles POST /api/v1/signals/:id/approve. Synchronous path:
//   - atomic CAS pending → approved (state machine)
//   - hand off to submitter (executor) which creates the Trade row
//   - on success, CAS approved → submitted with trade_id
//   - on failure, CAS approved → failed with error message
//
// Errors:
//   - 404 if pending_signal not found
//   - 409 if not in pending state (already decided)
//   - 410 if expired
//   - 500 on internal error
//   - 502 if submitter failed (approval succeeded, downstream did not)
func (h *ApprovalHandler) Approve(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	d := decisionOf(c)
	ctx := c.Request.Context()

	// Edge 1: PENDING → APPROVED (atomic CAS + persist provenance).
	ps, err := h.sm.Approve(ctx, id, d)
	if err != nil {
		respondStateErr(c, err)
		return
	}

	// Edge 2: APPROVED → EXECUTING (claim the work). On a process
	// crash between here and MarkSubmitted, the row stays in
	// `executing` and the sweeper's crash-recovery sweep transitions
	// it to FAILED so the operator can reconcile against the exchange.
	if err := h.sm.MarkExecuting(ctx, ps.ID); err != nil {
		log.Printf("[approval] id=%d mark-executing err: %v", ps.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not claim execution slot"})
		return
	}

	// Edge 3: EXECUTING → SUBMITTED or FAILED.
	tradeID, submitErr := h.submitter.Submit(c, ps.ID)
	if submitErr != nil {
		if markErr := h.sm.MarkFailed(ctx, ps.ID, submitErr.Error()); markErr != nil {
			log.Printf("[approval] id=%d mark-failed err: %v", ps.ID, markErr)
		}
		log.Printf("[approval] id=%d submit err: %v", ps.ID, submitErr)
		c.JSON(http.StatusBadGateway, gin.H{
			"error":             "submission failed",
			"detail":            submitErr.Error(),
			"pending_signal_id": ps.ID,
		})
		return
	}

	if err := h.sm.MarkSubmitted(ctx, ps.ID, tradeID); err != nil {
		// Trade exists locally but state machine row drifted (likely
		// the sweeper claimed it). Audit + 200; operator can reconcile.
		log.Printf("[approval] id=%d mark-submitted err: %v (trade %d already created)", ps.ID, err, tradeID)
	}

	h.broadcast("signal.approved", gin.H{"pending_signal_id": ps.ID, "trade_id": tradeID})

	c.JSON(http.StatusOK, gin.H{
		"status":            "submitted",
		"pending_signal_id": ps.ID,
		"trade_id":          tradeID,
	})
}

// Reject handles POST /api/v1/signals/:id/reject. Same error semantics
// as Approve except no submitter call.
func (h *ApprovalHandler) Reject(c *gin.Context) {
	id, ok := parseID(c)
	if !ok {
		return
	}
	d := decisionOf(c)

	var body rejectBody
	_ = c.ShouldBindJSON(&body) // body optional
	reason := body.Reason
	if reason == "" {
		reason = "user-rejected"
	}

	ps, err := h.sm.Reject(c.Request.Context(), id, reason, d)
	if err != nil {
		respondStateErr(c, err)
		return
	}
	h.broadcast("signal.rejected", gin.H{"pending_signal_id": ps.ID, "reason": reason})

	c.JSON(http.StatusOK, gin.H{
		"status":            "rejected",
		"pending_signal_id": ps.ID,
		"reason":            reason,
	})
}

// parseID extracts and validates :id from the path; writes 400 on
// failure and returns ok=false so the caller can early-return.
func parseID(c *gin.Context) (uint, bool) {
	raw := c.Param("id")
	parsed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || parsed == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
		return 0, false
	}
	return uint(parsed), true
}

// decisionOf builds the forensics record for a user-initiated edge.
// Reads identifier + ip + ua from gin context (set by bearer middleware
// in production; defaulted in tests that bypass middleware).
func decisionOf(c *gin.Context) approval.Decision {
	return approval.Decision{
		By:        contextString(c, "operator", "anonymous"),
		Source:    "api",
		IP:        contextString(c, "operator_ip", c.ClientIP()),
		UserAgent: contextString(c, "operator_user_agent", c.GetHeader("User-Agent")),
	}
}

func contextString(c *gin.Context, key, fallback string) string {
	if v, ok := c.Get(key); ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return fallback
}

// respondStateErr maps state-machine sentinel errors to HTTP status codes.
func respondStateErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, approval.ErrNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "pending signal not found"})
	case errors.Is(err, approval.ErrNotPending):
		c.JSON(http.StatusConflict, gin.H{"error": "pending signal already decided"})
	case errors.Is(err, approval.ErrExpired):
		c.JSON(http.StatusGone, gin.H{"error": "pending signal expired"})
	default:
		log.Printf("[approval] state err: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal"})
	}
}
