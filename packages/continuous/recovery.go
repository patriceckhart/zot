package continuous

import (
	"context"
	"errors"
	"fmt"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
)

// RecoveryAction describes what the next Step would do for one run. It is
// derived from committed state alone and performs no work. Policies of tools
// are reported as stored; the live registry may still deny a replay later.
type RecoveryAction struct {
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
	Phase          string `json:"phase"`
	Turn           int    `json:"turn"`
	Attempt        int    `json:"attempt"`
	// Action is resend, continue, replay, reconcile, report, approve, or abort.
	Action string `json:"action"`
	// Detail explains the decision in user language.
	Detail string `json:"detail"`
	// Automatic is false when a human should look before stepping.
	Automatic bool `json:"automatic"`
	// Interrupted lists tool calls whose effect is unknown.
	Interrupted    []ToolIntent `json:"interrupted,omitempty"`
	AbortRequested bool         `json:"abort_requested,omitempty"`
}

// RecoveryPlan lists every unfinished run in the store.
type RecoveryPlan struct {
	Revision uint64           `json:"revision"`
	Actions  []RecoveryAction `json:"actions"`
	// Blocked counts actions that are not automatic.
	Blocked int `json:"blocked"`
}

// RecoveryPreview scans every conversation for an unfinished run and reports
// what recovery would do. It commits nothing and acquires no new epoch beyond
// the open store's.
func (r *Runtime) RecoveryPreview(ctx context.Context) (RecoveryPlan, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return RecoveryPlan{}, err
	}
	plan := RecoveryPlan{Revision: snap.Revision(), Actions: []RecoveryAction{}}
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return plan, err
		}
		page, err := snap.Page("run/", after, 200)
		if err != nil {
			return plan, err
		}
		if len(page) == 0 {
			return plan, nil
		}
		for _, row := range page {
			after = row.Key
			run, _, err := read[Run](snap, row.Key)
			if err != nil {
				return plan, err
			}
			if run.Phase == "done" {
				continue
			}
			pending, err := pendingApprovals(snap, run.ConversationID)
			if err != nil {
				return plan, err
			}
			action := planRun(run, len(pending) > 0)
			if !action.Automatic {
				plan.Blocked++
			}
			plan.Actions = append(plan.Actions, action)
		}
	}
}

func planRun(run Run, awaitingApproval bool) RecoveryAction {
	a := RecoveryAction{ConversationID: run.ConversationID, RunID: run.ID, Phase: run.Phase, Turn: run.Turn, Attempt: run.Attempt, Automatic: true, AbortRequested: run.AbortRequested}
	if awaitingApproval && !run.AbortRequested {
		a.Action = "approve"
		a.Automatic = false
		a.Detail = "a tool call is waiting for a human approval decision; nothing runs until it is decided or the run is aborted"
		return a
	}
	if run.AbortRequested {
		a.Action = "abort"
		a.Detail = "abort was requested; unfinished tool calls receive aborted results and the inputs settle aborted"
		for _, t := range run.Tools {
			if t.State == "running" {
				a.Interrupted = append(a.Interrupted, t)
			}
		}
		return a
	}
	switch run.Phase {
	case "request":
		a.Action = "resend"
		a.Detail = fmt.Sprintf("the model request of turn %d attempt %d is sent again; the provider may have charged for an earlier attempt", run.Turn, run.Attempt)
	case "tools":
		a.Action = "continue"
		a.Detail = "pending tool calls run after authorization"
		for _, t := range run.Tools {
			if t.State != "running" {
				continue
			}
			a.Interrupted = append(a.Interrupted, t)
			switch t.Replay {
			case core.ReplaySafe:
				a.Action = "replay"
				a.Detail = "an interrupted read-only tool is executed again after fresh authorization"
			case core.ReplayIdempotent:
				a.Action = "replay"
				a.Detail = "an interrupted idempotent tool is executed again with its original operation key after fresh authorization; the external receiver must enforce the key"
			case core.ReplayReconcile:
				a.Action = "reconcile"
				a.Detail = "the tool is asked whether the interrupted operation completed; completed recovers the result, not started executes it, unknown is reported to the model"
			default:
				a.Action = "report"
				a.Automatic = false
				a.Detail = "an interrupted tool with unknown effect is reported to the model as an error and not repeated; inspect before stepping"
			}
		}
	default:
		a.Action = "report"
		a.Automatic = false
		a.Detail = "unknown run phase, the store needs inspection"
	}
	return a
}

var ErrNoRun = errors.New("continuous conversation has no active run")

// Abort records abort intent for the active run. The commit is the decision;
// the running stepper, or the next one, carries it out at the next boundary
// and settles the run aborted. Abort never cancels an external effect that
// already started and never deletes queued submissions.
func (r *Runtime) Abort(ctx context.Context, conversationID string) (Run, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Run{}, err
		}
		run, ok, err := read[Run](snap, runKey(conversationID))
		if err != nil {
			return Run{}, err
		}
		if !ok || run.Phase == "done" {
			return run, ErrNoRun
		}
		if run.AbortRequested {
			return run, nil
		}
		c, err := conversation(snap, conversationID)
		if err != nil {
			return Run{}, err
		}
		run.AbortRequested = true
		c.Revision = snap.Revision() + 1
		run.Revision = c.Revision
		ops := []storage.Operation{record("conversation/"+c.ID, c), record(runKey(c.ID), run)}
		// Cascade bottom-up: every conversation owned by a tool call of this
		// run that has an active run is marked too, in the same commit.
		cascade, err := ownedRunAbortOps(snap, run)
		if err != nil {
			return Run{}, err
		}
		ops = append(ops, cascade...)
		err = r.commit(ctx, snap, "run.abort.request", ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return run, err
	}
}

// ownedRunAbortOps marks abort on the active runs of conversations owned by
// the given run's tool calls, recursively. Terminal runs are skipped.
func ownedRunAbortOps(snap storage.Snapshot, run Run) ([]storage.Operation, error) {
	var ops []storage.Operation
	seen := map[string]bool{run.ConversationID: true}
	var walk func(parent Run) error
	walk = func(parent Run) error {
		for _, intent := range parent.Tools {
			ownerID := ToolCallIdentity{RunID: parent.ID, ConversationID: parent.ConversationID, CallID: intent.CallID}.OwnerID()
			children, err := ownedConversations(snap, ownerID)
			if err != nil {
				return err
			}
			for _, childID := range children {
				if seen[childID] {
					return fmt.Errorf("%w: ownership cycle", storage.ErrCorrupt)
				}
				seen[childID] = true
				childRun, ok, err := read[Run](snap, runKey(childID))
				if err != nil {
					return err
				}
				if !ok || childRun.Phase == "done" {
					continue
				}
				if err := walk(childRun); err != nil {
					return err
				}
				if childRun.AbortRequested {
					continue
				}
				c, err := conversation(snap, childID)
				if err != nil {
					return err
				}
				childRun.AbortRequested = true
				c.Revision = snap.Revision() + 1
				childRun.Revision = c.Revision
				ops = append(ops, record("conversation/"+childID, c), record(runKey(childID), childRun))
			}
		}
		return nil
	}
	if err := walk(run); err != nil {
		return nil, err
	}
	return ops, nil
}
