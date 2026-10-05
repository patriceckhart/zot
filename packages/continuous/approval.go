package continuous

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/provider"
)

// Approval is a persisted human decision point for one tool call. It is
// committed before anyone is notified, so a crash, reconnect, or second
// client attaches to the same pending record instead of asking twice. The
// decision binds to the normalized arguments: a different call needs a new
// approval. Nothing is approved implicitly; a missing decision keeps the
// run parked.
type Approval struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
	CallID         string `json:"call_id"`
	Tool           string `json:"tool"`
	// Args are the effective arguments the decision applies to. ArgsHash is
	// their SHA-256 so a decision for different arguments is detected.
	Args     json.RawMessage `json:"args"`
	ArgsHash string          `json:"args_hash"`
	Summary  string          `json:"summary,omitempty"`
	// State is pending, allowed, denied, or expired.
	State string `json:"state"`
	// Actor and Reason record who decided and why. Scope is call (default)
	// or tool: a tool-scoped allow also covers later calls of the same tool
	// in this conversation until the policy revision changes.
	Actor  string `json:"actor,omitempty"`
	Reason string `json:"reason,omitempty"`
	Scope  string `json:"scope,omitempty"`
	// PolicyRevision is the conversation revision the approval was requested
	// at. A tool-scoped allow is honoured only while the conversation's
	// configuration revision is unchanged.
	PolicyRevision uint64     `json:"policy_revision"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	Created        time.Time  `json:"created"`
	Decided        *time.Time `json:"decided,omitempty"`
	Revision       uint64     `json:"revision"`
}

// ApprovalRequest is what an Approver returns to require a decision.
type ApprovalRequest struct {
	// Summary is a short human description of the effect.
	Summary string
	// TTL bounds how long the request stays decidable. Zero means no expiry.
	TTL time.Duration
}

// Approver decides, before the intent commit, whether a call needs a human
// decision. It runs after the host's BeforeToolExecute guard allowed the
// call. Returning ok false means no approval is needed. It must not perform
// the call's effect or block on a human; the decision arrives through
// Runtime.Decide.
type Approver func(ctx context.Context, c Conversation, call provider.ToolCallBlock, args json.RawMessage) (req ApprovalRequest, ok bool)

var ErrAwaitingApproval = errors.New("continuous run is waiting for an approval decision")
var ErrApprovalNotFound = errors.New("continuous approval not found")
var ErrApprovalDecided = errors.New("continuous approval already decided")

const (
	approvalPending = "pending"
	approvalAllowed = "allowed"
	approvalDenied  = "denied"
	approvalExpired = "expired"
)

func approvalKey(id string) string { return "approval/" + id }
func approvalConversationKey(conversationID, id string) string {
	return "approval-conversation/" + conversationID + "/" + id
}
func argsHash(args json.RawMessage) string {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		sum := sha256.Sum256(args)
		return hex.EncodeToString(sum[:])
	}
	normalized, _ := json.Marshal(v)
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:])
}

// Approval reads one approval.
func (r *Runtime) Approval(ctx context.Context, id string) (Approval, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Approval{}, err
	}
	a, ok, err := read[Approval](snap, approvalKey(id))
	if err != nil {
		return Approval{}, err
	}
	if !ok {
		return Approval{}, ErrApprovalNotFound
	}
	return a, nil
}

// PendingApprovals lists undecided approvals of a conversation, oldest first.
func (r *Runtime) PendingApprovals(ctx context.Context, conversationID string) ([]Approval, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return pendingApprovals(snap, conversationID)
}

func pendingApprovals(snap storage.Snapshot, conversationID string) ([]Approval, error) {
	rows, err := snap.Page(approvalConversationKey(conversationID, ""), "", 1000)
	if err != nil {
		return nil, err
	}
	var out []Approval
	for _, row := range rows {
		var id string
		if json.Unmarshal(row.Value, &id) != nil {
			return nil, storage.ErrCorrupt
		}
		a, ok, err := read[Approval](snap, approvalKey(id))
		if err != nil || !ok {
			return nil, fmt.Errorf("%w: approval index without record", storage.ErrCorrupt)
		}
		if a.State == approvalPending {
			out = append(out, a)
		}
	}
	return out, nil
}

// Decide records a decision. allow true approves the call with the recorded
// arguments; false denies it. scope is "call" or "tool". The commit is the
// decision; the parked run continues at its next step. Deciding an approval
// twice, or after it expired, is an error, never a silent overwrite.
func (r *Runtime) Decide(ctx context.Context, id, actor string, allow bool, scope, reason string) (Approval, error) {
	if scope == "" {
		scope = "call"
	}
	if scope != "call" && scope != "tool" {
		return Approval{}, fmt.Errorf("approval scope must be call or tool")
	}
	if actor == "" {
		return Approval{}, fmt.Errorf("approval decision requires an actor")
	}
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Approval{}, err
		}
		a, ok, err := read[Approval](snap, approvalKey(id))
		if err != nil {
			return Approval{}, err
		}
		if !ok {
			return Approval{}, ErrApprovalNotFound
		}
		if a.State != approvalPending {
			return a, ErrApprovalDecided
		}
		now := time.Now().UTC()
		if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
			a.State = approvalExpired
			a.Revision = snap.Revision() + 1
			if err := r.commit(ctx, snap, "approval.expire", record(approvalKey(id), a)); err != nil && !errors.Is(err, storage.ErrConflict) {
				return a, err
			}
			continue
		}
		a.State = approvalDenied
		if allow {
			a.State = approvalAllowed
		}
		a.Actor, a.Reason, a.Scope, a.Decided = actor, reason, scope, &now
		a.Revision = snap.Revision() + 1
		err = r.commit(ctx, snap, "approval.decide:"+actor, record(approvalKey(id), a))
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return a, err
	}
}

// approvalFor resolves the approval state of a call. It returns the pending
// approval to park on, or whether a decision allows or denies the call. An
// allowed call-scoped approval is consumed by the intent commit (it stays
// allowed as audit, the intent records its ID). Tool-scoped allows apply to
// later calls while the conversation revision at request time is unchanged.
func approvalFor(snap storage.Snapshot, c Conversation, run Run, call provider.ToolCallBlock, args json.RawMessage) (pending *Approval, allowed bool, decided *Approval, err error) {
	rows, err := snap.Page(approvalConversationKey(c.ID, ""), "", 1000)
	if err != nil {
		return nil, false, nil, err
	}
	hash := argsHash(args)
	now := time.Now().UTC()
	for _, row := range rows {
		var id string
		if json.Unmarshal(row.Value, &id) != nil {
			return nil, false, nil, storage.ErrCorrupt
		}
		a, ok, err := read[Approval](snap, approvalKey(id))
		if err != nil || !ok {
			return nil, false, nil, fmt.Errorf("%w: approval index without record", storage.ErrCorrupt)
		}
		if a.Tool != call.Name {
			continue
		}
		exact := a.RunID == run.ID && a.CallID == call.ID && a.ArgsHash == hash
		switch a.State {
		case approvalPending:
			if exact {
				if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
					// Expired without a decision: treated as no approval. The
					// stepper re-requests so a human sees a fresh record.
					continue
				}
				pending = &a
				return pending, false, nil, nil
			}
		case approvalDenied:
			if exact {
				return nil, false, &a, nil
			}
		case approvalAllowed:
			if exact {
				return nil, true, &a, nil
			}
			if a.Scope == "tool" && a.PolicyRevision == c.ConfigRevision {
				return nil, true, &a, nil
			}
		}
	}
	return nil, false, nil, nil
}

// requestApproval commits a pending approval for the call and parks the run.
// The intent stays pending; nothing executes. Idempotent: an identical
// pending request is returned, not duplicated.
func (s *Service) requestApproval(ctx context.Context, snap storage.Snapshot, c Conversation, run Run, call provider.ToolCallBlock, args json.RawMessage, req ApprovalRequest) (Approval, error) {
	a := Approval{ID: uuid.NewString(), ConversationID: c.ID, RunID: run.ID, CallID: call.ID, Tool: call.Name, Args: args, ArgsHash: argsHash(args), Summary: req.Summary, State: approvalPending, PolicyRevision: c.ConfigRevision, Created: time.Now().UTC(), Revision: snap.Revision() + 1}
	if req.TTL > 0 {
		exp := a.Created.Add(req.TTL)
		a.ExpiresAt = &exp
	}
	run.Notices = append(run.Notices, fmt.Sprintf("tool %s is waiting for approval %s", call.ID, a.ID))
	// The notification commits with the pending record, so no approval can
	// exist that nobody is told about, and no one is told about an approval
	// that does not exist.
	notify := outboxOp(approvalNotificationID(a.ID), "approval.pending", c.ID, fmt.Sprintf("approval needed: %s", firstNonEmpty(req.Summary, call.Name)), map[string]any{"approval_id": a.ID, "tool": call.Name, "run_id": run.ID})
	_, err := s.commitRun(ctx, snap, c, run, "approval.request", record(approvalKey(a.ID), a), record(approvalConversationKey(c.ID, a.ID), a.ID), notify)
	if err != nil {
		return a, err
	}
	s.opts.Sink(EvApproval{Approval: a})
	return a, nil
}

// EvApproval is published when a pending approval is committed or decided.
type EvApproval struct{ Approval Approval }

func (EvApproval) Type() string { return "approval" }
