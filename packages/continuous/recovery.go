package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/provider"
	"time"

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
	// Executor is "tasks" for a generation chain, empty for an unmigrated
	// run of an earlier build.
	Executor string `json:"executor,omitempty"`
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
	if err := pageAll(snap, "chain/", func(row storage.Record) error {
		var chain Chain
		if json.Unmarshal(row.Value, &chain) != nil {
			return storage.ErrCorrupt
		}
		action := planChain(snap, chain)
		if !action.Automatic {
			plan.Blocked++
		}
		plan.Actions = append(plan.Actions, action)
		return nil
	}); err != nil {
		return plan, err
	}
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

// Abort stops a conversation's work. An active chain is aborted as a task
// abort of its generation and, bottom-up, the tool tasks and owned
// conversations it is responsible for; a started effect is joined, not
// cancelled. An unfinished run of the earlier executor that could not be
// migrated is settled in one commit: every unfinished call gets an aborted
// result, its approvals expire, and its inputs settle aborted. Queued inputs
// stay queued.
func (r *Runtime) Abort(ctx context.Context, conversationID string) (Run, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Run{}, err
		}
		if chain, ok, err := read[Chain](snap, chainKey(conversationID)); err != nil {
			return Run{}, err
		} else if ok {
			if err := r.AbortTask(ctx, chain.Task, false); err != nil {
				return Run{}, err
			}
			snap, err := r.store.Snapshot(ctx)
			if err != nil {
				return Run{}, err
			}
			return chainRun(snap, chain), nil
		}
		run, ok, err := read[Run](snap, runKey(conversationID))
		if err != nil {
			return Run{}, err
		}
		if !ok || run.Phase == "done" {
			return run, ErrNoRun
		}
		ops, settled, err := abortLegacyOps(snap, run)
		if err != nil {
			return Run{}, err
		}
		err = r.commit(ctx, snap, "run.abort", ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return settled, err
	}
}

// abortLegacyOps settles an unmigrated run of the earlier executor as
// aborted, keeping tool calls and results paired.
func abortLegacyOps(snap storage.Snapshot, run Run) ([]storage.Operation, Run, error) {
	c, err := conversation(snap, run.ConversationID)
	if err != nil {
		return nil, run, err
	}
	var ops []storage.Operation
	for i, intent := range run.Tools {
		if intent.State == "done" {
			continue
		}
		text := "aborted: the run was aborted before this tool call started."
		if intent.State == "running" {
			text = "aborted: the run was aborted after this tool call started. Its effect is unknown."
			run.Notices = append(run.Notices, fmt.Sprintf("tool %s aborted while running, effect unknown", intent.CallID))
		}
		block := provider.ToolResultBlock{CallID: intent.CallID, Content: []provider.Content{provider.TextBlock{Text: text}}, IsError: true}
		c.EntrySequence++
		run.Tools[i].State, run.Tools[i].Entry = "done", c.EntrySequence
		entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, Revision: snap.Revision() + 1, Type: entryToolResult, Content: text, Time: time.Now().UTC()}
		entry.Message = marshalMessage(provider.Message{Role: provider.RoleTool, Content: []provider.Content{block}, Time: time.Now().UTC()})
		ops = append(ops, record(entryKey(c.ID, c.EntrySequence), entry))
	}
	pending, err := pendingApprovals(snap, c.ID)
	if err != nil {
		return nil, run, err
	}
	for _, a := range pending {
		if a.RunID == run.ID {
			a.State, a.Reason, a.Revision = approvalExpired, "run aborted", snap.Revision()+1
			ops = append(ops, record(approvalKey(a.ID), a))
		}
	}
	run.Phase, run.Outcome, run.Error, run.AbortRequested = "done", "aborted", "aborted by request", true
	c.Revision = snap.Revision() + 1
	run.Revision = c.Revision
	ops = append(ops, settleOps(snap, run.Submissions, "aborted")...)
	ops = append(ops, record("conversation/"+c.ID, c), record(runKey(c.ID), run))
	return ops, run, nil
}
