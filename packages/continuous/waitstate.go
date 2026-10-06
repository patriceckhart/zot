package continuous

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// waitState tracks why a followed submission is not producing output, from
// committed state only: the submission's own state, pending approvals of
// the conversation, and a recovery hold present when following started.
type waitState struct {
	submission string
	state      string
	// approvals maps pending approval IDs to their tool names.
	approvals map[string]string
	recovery  string
}

func newWaitState(sub Submission, snap ConversationSnapshot) *waitState {
	ws := &waitState{submission: sub.ID, state: sub.State, approvals: map[string]string{}}
	for _, a := range snap.Approvals {
		ws.approvals[a.ID] = a.Tool
	}
	if r := snap.Recovery; r != nil && !r.Automatic && r.Action != "approve" {
		ws.recovery = r.Action
	}
	return ws
}

// apply folds a commit in and reports whether the description changed.
func (ws *waitState) apply(cm storage.Commit, conversationID string) bool {
	before := ws.describe()
	for _, op := range cm.Operations {
		switch {
		case op.Key == "submission/"+ws.submission && !op.Delete:
			var s Submission
			if json.Unmarshal(op.Value, &s) == nil {
				ws.state = s.State
			}
		case strings.HasPrefix(op.Key, "approval/") && !op.Delete:
			var a Approval
			if json.Unmarshal(op.Value, &a) != nil || a.ConversationID != conversationID {
				continue
			}
			if a.State == approvalPending {
				ws.approvals[a.ID] = a.Tool
			} else {
				delete(ws.approvals, a.ID)
			}
		case op.Key == chainKey(conversationID):
			// Any chain movement means the run is being stepped again.
			ws.recovery = ""
		}
	}
	return ws.describe() != before
}

// describe is the short status text, or "" when nothing is waiting.
func (ws *waitState) describe() string {
	if n := len(ws.approvals); n > 0 {
		tools := make([]string, 0, n)
		for _, t := range ws.approvals {
			tools = append(tools, t)
		}
		sort.Strings(tools)
		return "awaiting approval: " + strings.Join(tools, ", ") + " (zot continuous approvals)"
	}
	if ws.recovery != "" {
		return fmt.Sprintf("recovery blocked (%s): run `zot continuous recover --dry-run`", ws.recovery)
	}
	if ws.state == "queued" {
		return "queued on host"
	}
	return ""
}

// HistoryNotice describes a snapshot that holds only the newest page of
// a conversation's entries, or returns "" when it is complete.
func HistoryNotice(snap ConversationSnapshot, conversationID string) string {
	if !snap.More {
		return ""
	}
	return fmt.Sprintf("showing the newest %d entries of conversation %s; older history stays on the host (conversation.search)", len(snap.Entries), conversationID)
}

// ToolProgressFromCommit extracts committed tool progress records of a
// conversation's tasks. Progress is written by a task-scoped commit that
// also writes the task record, which names the conversation.
func ToolProgressFromCommit(cm storage.Commit, conversationID string) []ToolProgress {
	owned := map[string]bool{}
	for _, op := range cm.Operations {
		if op.Delete || !strings.HasPrefix(op.Key, "task/") {
			continue
		}
		var t Task
		if json.Unmarshal(op.Value, &t) == nil && t.ConversationID == conversationID {
			owned[t.ID] = true
		}
	}
	var out []ToolProgress
	for _, op := range cm.Operations {
		if op.Delete || !strings.HasPrefix(op.Key, "progress/") {
			continue
		}
		var p ToolProgress
		if json.Unmarshal(op.Value, &p) == nil && p.CallID != "" && owned[p.TaskID] {
			out = append(out, p)
		}
	}
	return out
}
