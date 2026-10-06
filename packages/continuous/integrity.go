package continuous

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
)

// Integrity reports checks of the current admission projection, not historical
// commits, tool execution, artifacts, or task ownership. Unknown record kinds
// fail closed. This provisional API must evolve alongside the runtime schema.
type Integrity struct {
	Valid         bool   `json:"valid"`
	Scope         string `json:"scope"`
	Revision      uint64 `json:"revision"`
	Conversations uint64 `json:"conversations"`
	Submissions   uint64 `json:"submissions"`
	Entries       uint64 `json:"entries"`
	ActiveRuns    uint64 `json:"active_runs"`
}

// CheckIntegrity validates one detached snapshot without committing or repairing
// records. It retains ID sets proportional to current entries and submissions.
// It does not make the writer or its snapshot memory-bounded. Errors identify
// invariant classes only, never private IDs, record keys, or content.
func (r *Runtime) CheckIntegrity(ctx context.Context) (Integrity, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Integrity{}, err
	}
	report := Integrity{Scope: "admission-state", Revision: snap.Revision()}
	bad := func(reason string) error {
		return fmt.Errorf("%w: admission integrity: %s", storage.ErrCorrupt, reason)
	}
	page := func(prefix string, visit func(storage.Record) error) error {
		after := ""
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			rows, err := snap.Page(prefix, after, 100)
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				return nil
			}
			for _, row := range rows {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := visit(row); err != nil {
					return err
				}
				after = row.Key
			}
		}
	}
	validRevision := func(n uint64) bool { return n > 0 && n <= snap.Revision() }
	validID := func(id string) bool {
		return id != "" && len(id) <= 128 && strings.TrimSpace(id) == id && !strings.ContainsAny(id, "/\\\x00\r\n")
	}
	validDigest := func(s string) bool {
		b, err := hex.DecodeString(s)
		return err == nil && len(b) == 32 && strings.ToLower(s) == s
	}
	getConversation := func(id string) (Conversation, error) {
		c, ok, err := read[Conversation](snap, "conversation/"+id)
		if err != nil || !ok || c.ID != id {
			return c, bad("missing or mismatched conversation")
		}
		return c, nil
	}
	getSubmission := func(id string) (Submission, error) {
		s, ok, err := read[Submission](snap, "submission/"+id)
		if err != nil || !ok || s.ID != id {
			return s, bad("missing or mismatched submission")
		}
		return s, nil
	}
	entryIDs := make(map[string]string)
	userEntries := make(map[string]bool)
	steerEntries := make(map[string]bool)
	queueSlots := make(map[string]string)
	runSubmissions := make(map[string]string)
	runs := make(map[string]Run)
	chains := make(map[string]Chain)
	submissionCounts := make(map[string]uint64)
	submissionMax := make(map[string]uint64)
	err = page("submission/", func(row storage.Record) error {
		var s Submission
		if json.Unmarshal(row.Value, &s) != nil {
			return bad("invalid submission")
		}
		submissionCounts[s.ConversationID]++
		submissionMax[s.ConversationID] = max(submissionMax[s.ConversationID], s.Sequence)
		return nil
	})
	if err != nil {
		return Integrity{}, err
	}
	err = page("run/", func(row storage.Record) error {
		var run Run
		id := strings.TrimPrefix(row.Key, "run/")
		if json.Unmarshal(row.Value, &run) != nil || !validID(run.ID) || run.ConversationID != id || !validRevision(run.Revision) || len(run.Submissions) == 0 {
			return bad("invalid run")
		}
		c, err := getConversation(id)
		if err != nil {
			return err
		}
		if run.Revision > c.Revision || run.Cutoff > c.EntrySequence || run.Turn < 1 || run.Attempt < 1 {
			return bad("run outside conversation state")
		}
		switch run.Phase {
		case "request":
			if len(run.Tools) != 0 || run.Outcome != "" {
				return bad("request phase with tools or outcome")
			}
		case "tools":
			if len(run.Tools) == 0 || run.Outcome != "" {
				return bad("tools phase without intents")
			}
			for _, intent := range run.Tools {
				switch intent.State {
				case "pending":
					if intent.Entry != 0 || len(intent.Args) != 0 {
						return bad("pending intent with execution data")
					}
				case "running":
					if intent.Entry != 0 || !json.Valid(intent.Args) || (intent.Replay != core.ReplayNever && intent.Replay != core.ReplaySafe && intent.Replay != core.ReplayIdempotent && intent.Replay != core.ReplayReconcile) {
						return bad("running intent without committed intent data")
					}
				case "done":
					if intent.Entry == 0 || intent.Entry > c.EntrySequence {
						return bad("done intent without result entry")
					}
				default:
					return bad("unknown intent state")
				}
			}
		case "done":
			if run.Outcome == "migrated" {
				// Continued as a generation chain, which owns its inputs.
				runs[id] = run
				return nil
			}
			if run.Outcome != "completed" && run.Outcome != "failed" && run.Outcome != "aborted" {
				return bad("done run without outcome")
			}
		default:
			return bad("unknown run phase")
		}
		if run.Phase != "done" {
			report.ActiveRuns++
		}
		for _, sid := range run.Submissions {
			if runSubmissions[sid] != "" {
				return bad("submission claimed by two runs")
			}
			runSubmissions[sid] = run.ID
			s, err := getSubmission(sid)
			if err != nil {
				return err
			}
			if s.ConversationID != id {
				return bad("run submission from another conversation")
			}
			expected := "running"
			if run.Phase == "done" {
				expected = "answered"
				if run.Outcome != "completed" {
					expected = run.Outcome
				}
			}
			if s.State != expected {
				return bad("submission state disagrees with run")
			}
		}
		runs[id] = run
		return nil
	})
	if err != nil {
		return Integrity{}, err
	}
	err = page("conversation/", func(row storage.Record) error {
		var c Conversation
		if json.Unmarshal(row.Value, &c) != nil || !validID(c.ID) || row.Key != "conversation/"+c.ID || c.Created.IsZero() || !validRevision(c.Revision) || c.QueueSequence > c.EntrySequence {
			return bad("invalid conversation")
		}
		info, imported, err := read[SessionImport](snap, "legacy/session/"+c.ID)
		if err != nil {
			return bad("invalid import provenance")
		}
		if c.Owner != nil {
			owner, err := getConversation(c.Owner.ConversationID)
			if err != nil {
				return err
			}
			if owner.ID == c.ID || c.Owner.ID == "" || owner.Created.After(c.Created) {
				return bad("conversation owner reference")
			}
			if _, ok := snap.Get(ownedConversationKey(c.Owner.ID, c.ID)); !ok {
				return bad("owned conversation without index")
			}
			// Ownership chains terminate.
			seen := map[string]bool{c.ID: true}
			for cur := owner; cur.Owner != nil; {
				if seen[cur.ID] {
					return bad("conversation ownership cycle")
				}
				seen[cur.ID] = true
				next, err := getConversation(cur.Owner.ConversationID)
				if err != nil {
					return err
				}
				cur = next
			}
		}
		if c.Parent != nil {
			parent, err := getConversation(c.Parent.ConversationID)
			if err != nil {
				return err
			}
			if c.Parent.At == 0 || c.Parent.At > parent.EntrySequence || parent.ID == c.ID || parent.Created.After(c.Created) {
				return bad("fork parent reference")
			}
			if imported {
				return bad("fork with import provenance")
			}
		}
		// Queue slots are keyed by admitted sequence; reordering rewrites
		// which queued submission occupies a slot, withdrawing removes a
		// slot. Each slot references a distinct queued submission of this
		// conversation, no slot exceeds the queue counter, and every queued
		// submission is referenced exactly once (checked per submission).
		var queued, entries, claimed uint64
		slotIDs := map[string]bool{}
		if err := page("queue/"+c.ID+"/", func(row storage.Record) error {
			queued++
			var id string
			if json.Unmarshal(row.Value, &id) != nil {
				return bad("invalid queue reference")
			}
			s, err := getSubmission(id)
			if err != nil {
				return err
			}
			var slot uint64
			if _, err := fmt.Sscanf(strings.TrimPrefix(row.Key, "queue/"+c.ID+"/"), "%d", &slot); err != nil || slot == 0 || slot > c.QueueSequence {
				return bad("queue slot outside admitted sequences")
			}
			if s.ConversationID != c.ID || s.State != "queued" || slotIDs[id] {
				return bad("queue submission mismatch")
			}
			slotIDs[id] = true
			queueSlots[id] = row.Key
			return nil
		}); err != nil {
			return err
		}
		if claimed = submissionCounts[c.ID]; claimed != c.QueueSequence || submissionMax[c.ID] != c.QueueSequence && c.QueueSequence != 0 {
			return bad("queue counter mismatch")
		}
		var usageRows uint64
		if err := page("usage/"+c.ID+"/", func(row storage.Record) error {
			usageRows++
			var rec UsageRecord
			if json.Unmarshal(row.Value, &rec) != nil || rec.ConversationID != c.ID || row.Key != usageKey(c.ID, usageRows) || !validID(rec.RunID) || rec.Turn < 1 || rec.Attempt < 1 || (rec.Status != "known" && rec.Status != "unknown" && rec.Status != "estimated") || (rec.Source != "model" && rec.Source != "tool") {
				return bad("invalid usage ledger row")
			}
			return nil
		}); err != nil {
			return err
		}
		if usageRows != c.UsageSequence {
			return bad("usage counter mismatch")
		}
		if err := page("entry/"+c.ID+"/", func(row storage.Record) error {
			entries++
			var e Entry
			if json.Unmarshal(row.Value, &e) != nil || !validID(e.ID) || e.ConversationID != c.ID || row.Key != entryKey(c.ID, entries) || !validRevision(e.Revision) || e.Revision > c.Revision || entryIDs[e.ID] != "" {
				return bad("invalid or duplicate entry")
			}
			entryIDs[e.ID] = row.Key
			if (imported && info.Rows > 0 && entries <= uint64(info.Rows)) != strings.HasPrefix(e.Type, "legacy_") {
				return bad("import entry range mismatch")
			}
			switch {
			case e.Type == entryReset:
				if e.SubmissionID != "" || len(e.Message) != 0 || len(e.LegacyRow) != 0 || len(e.SessionProjection) != 0 {
					return bad("reset entry with foreign fields")
				}
			case e.Type == entryContinue:
				if e.SubmissionID != "" || len(e.Message) != 0 || len(e.LegacyRow) != 0 || len(e.SessionProjection) != 0 || strings.TrimSpace(e.Content) == "" {
					return bad("continue entry with foreign fields")
				}
			case e.Type == entryContext:
				var cc ContextChange
				if e.SubmissionID != "" || len(e.Message) != 0 || len(e.LegacyRow) != 0 || len(e.SessionProjection) != 0 || json.Unmarshal(e.Data, &cc) != nil || (cc.System == nil && cc.Tools == nil) {
					return bad("invalid context entry")
				}
			case e.Type == entryCompaction:
				var info CompactionInfo
				if e.SubmissionID != "" || len(e.LegacyRow) != 0 || len(e.SessionProjection) != 0 || json.Unmarshal(e.Data, &info) != nil || info.Head == 0 || info.Head > entries || (info.Reason != "manual" && info.Reason != "threshold" && info.Reason != "overflow" && info.Reason != "background") {
					return bad("invalid compaction entry")
				}
				if msg, err := core.DecodeMessage(e.Message); err != nil || msg.Role != "user" {
					return bad("compaction summary message")
				}
			case e.Type == entryAssistant || e.Type == entryToolResult || e.Type == entryAttempt:
				if e.SubmissionID != "" || len(e.LegacyRow) != 0 || len(e.SessionProjection) != 0 {
					return bad("execution entry with foreign fields")
				}
				if len(e.Message) > 0 {
					msg, err := core.DecodeMessage(e.Message)
					if err != nil {
						return bad("undecodable execution message")
					}
					switch e.Type {
					case entryAssistant:
						if msg.Role != "assistant" {
							return bad("assistant entry with wrong role")
						}
					case entryToolResult:
						if msg.Role != "tool" || len(msg.Content) != 1 {
							return bad("tool result entry shape")
						}
					}
				} else if e.Type != entryAttempt {
					return bad("execution entry without message")
				}
			case e.Type == "user":
				s, err := getSubmission(e.SubmissionID)
				if err != nil {
					return err
				}
				if s.ConversationID != c.ID || s.Content != e.Content || s.Revision != e.Revision || userEntries[s.ID] || len(e.LegacyRow) != 0 || len(e.SessionProjection) != 0 {
					return bad("user entry submission mismatch")
				}
				userEntries[s.ID] = true
			case e.Type == entrySteer:
				s, err := getSubmission(e.SubmissionID)
				if err != nil {
					return err
				}
				if s.ConversationID != c.ID || s.Content != e.Content || s.State == "queued" || s.State == "withdrawn" || !userEntries[s.ID] || steerEntries[s.ID] || len(e.Message) != 0 {
					return bad("steer entry submission mismatch")
				}
				steerEntries[s.ID] = true
			case strings.HasPrefix(e.Type, "legacy_"):
				var rowType struct {
					Type string `json:"type"`
				}
				if e.SubmissionID != "" || json.Unmarshal(e.LegacyRow, &rowType) != nil || rowType.Type == "" || e.Type != "legacy_"+rowType.Type {
					return bad("invalid legacy entry")
				}
				if _, ok := snap.Get("legacy/session/" + c.ID); !ok {
					return bad("missing import provenance")
				}
			default:
				return bad("unsupported entry type")
			}
			report.Entries++
			return nil
		}); err != nil {
			return err
		}
		if entries != c.EntrySequence {
			return bad("entry counter mismatch")
		}
		if run, ok := runs[c.ID]; ok && run.Phase == "tools" {
			// During a round the cutoff is the tool-calling assistant entry and
			// every finished intent points at a later tool result entry.
			assistant, ok, err := read[Entry](snap, entryKey(c.ID, run.Cutoff))
			if err != nil || !ok || assistant.Type != entryAssistant {
				return bad("tool round without assistant entry")
			}
			for _, intent := range run.Tools {
				if intent.State != "done" {
					continue
				}
				result, ok, err := read[Entry](snap, entryKey(c.ID, intent.Entry))
				if err != nil || !ok || intent.Entry <= run.Cutoff || result.Type != entryToolResult {
					return bad("intent result entry mismatch")
				}
			}
		}
		report.Conversations++
		return nil
	})
	if err != nil {
		return Integrity{}, err
	}
	err = page("chain/", func(row storage.Record) error {
		var chain Chain
		id := strings.TrimPrefix(row.Key, "chain/")
		if json.Unmarshal(row.Value, &chain) != nil || chain.ConversationID != id || !validID(chain.RunID) || !validRevision(chain.Revision) || len(chain.Submissions) == 0 {
			return bad("invalid generation chain")
		}
		if _, err := getConversation(id); err != nil {
			return err
		}
		if _, ok := runs[id]; ok && runs[id].Phase != "done" {
			return bad("conversation with an unmigrated run and a chain")
		}
		chains[id] = chain
		t, ok, err := read[Task](snap, taskKey(chain.Task))
		if err != nil || !ok || t.Kind != TaskKindGeneration || t.ConversationID != id || t.State == "terminal" {
			return bad("generation chain without live generation task")
		}
		for _, sid := range chain.Submissions {
			if runSubmissions[sid] != "" {
				return bad("submission claimed twice")
			}
			runSubmissions[sid] = chain.RunID
			sub, err := getSubmission(sid)
			if err != nil {
				return err
			}
			if sub.ConversationID != id || sub.State != "running" {
				return bad("chain submission state mismatch")
			}
		}
		report.ActiveRuns++
		return nil
	})
	if err != nil {
		return Integrity{}, err
	}
	err = page("", func(row storage.Record) error {
		switch {
		case row.Key == compactionStatsKey:
			var m CompactionMetrics
			if json.Unmarshal(row.Value, &m) != nil {
				return bad("invalid compaction stats")
			}
		case row.Key == runtimeFormatKey:
			var f RuntimeFormat
			if json.Unmarshal(row.Value, &f) != nil || f.Version < 1 || f.Version > RuntimeFormatVersion || !validRevision(f.Revision) {
				return bad("invalid runtime format")
			}
		case strings.HasPrefix(row.Key, "chain/"):
		case strings.HasPrefix(row.Key, "bgcompaction/"):
			var id string
			if json.Unmarshal(row.Value, &id) != nil {
				return bad("invalid background compaction reference")
			}
			t, ok, err := read[Task](snap, taskKey(id))
			if err != nil || !ok || t.Kind != TaskKindCompaction || row.Key != backgroundKey(t.ConversationID) {
				return bad("background compaction reference without its task")
			}
		case strings.HasPrefix(row.Key, "progress/"):
			var p ToolProgress
			if json.Unmarshal(row.Value, &p) != nil || row.Key != progressKey(p.TaskID) || len(p.Text) > MaxToolProgressBytes {
				return bad("invalid tool progress")
			}
			t, ok, err := read[Task](snap, taskKey(p.TaskID))
			if err != nil || !ok || t.Kind != TaskKindTool || t.State == "terminal" {
				return bad("tool progress without its running task")
			}
		case strings.HasPrefix(row.Key, "executor/"):
			// Written by an earlier build that switched executors per
			// conversation. There is only one executor now; the record is
			// inert and kept for audit.
		case strings.HasPrefix(row.Key, "conversation/"), strings.HasPrefix(row.Key, "run/"), strings.HasPrefix(row.Key, "usage/"):
		case strings.HasPrefix(row.Key, "outbox/"):
			var n Notification
			if json.Unmarshal(row.Value, &n) != nil || n.ID == "" || row.Key != outboxKey(n.ID) || n.Kind == "" || n.Created.IsZero() || n.Attempts < 0 {
				return bad("invalid outbox notification")
			}
		case strings.HasPrefix(row.Key, "memo/"):
			var m Memo
			if json.Unmarshal(row.Value, &m) != nil || m.Scope == "" || m.Key == "" || row.Key != memoKey(m.Scope, m.Key) || !validRevision(m.Revision) || m.Created.IsZero() {
				return bad("invalid memo")
			}
		case strings.HasPrefix(row.Key, "prompt-section/"):
			var p PromptSection
			if json.Unmarshal(row.Value, &p) != nil || row.Key != promptSectionKey(p.Hash) || contentHash([]byte(p.Text)) != p.Hash || (p.Kind != "system" && p.Kind != "tools") {
				return bad("invalid prompt section")
			}
		case strings.HasPrefix(row.Key, "prompt/"):
			var p PromptRecord
			if json.Unmarshal(row.Value, &p) != nil || row.Key != promptRecordKey(p.ConversationID, p.RunID, p.Turn, p.Attempt) || p.Time.IsZero() {
				return bad("invalid prompt record")
			}
			if _, err := getConversation(p.ConversationID); err != nil {
				return err
			}
			for _, h := range []string{p.SystemHash, p.ToolsHash} {
				if _, ok := snap.Get(promptSectionKey(h)); !ok {
					return bad("prompt record without its section")
				}
			}
		case strings.HasPrefix(row.Key, "partial/"):
			var p Partial
			if json.Unmarshal(row.Value, &p) != nil || row.Key != partialKey(p.ConversationID) || !validID(p.RunID) || p.Turn < 1 || p.Attempt < 1 || p.Updated.IsZero() {
				return bad("invalid partial output record")
			}
			if _, err := getConversation(p.ConversationID); err != nil {
				return err
			}
			if chain, ok := chains[p.ConversationID]; ok && chain.RunID == p.RunID {
				return nil
			}
			if p.Final {
				return nil
			}
			run, ok := runs[p.ConversationID]
			if !ok || run.ID != p.RunID {
				return bad("partial output without its run")
			}
			if !p.Final && run.Phase != "request" {
				return bad("live partial output outside a request")
			}
		case row.Key == "budget/runtime" || strings.HasPrefix(row.Key, "budget/conversation/"):
			var b Budget
			if json.Unmarshal(row.Value, &b) != nil || row.Key != budgetKey(b.Scope, b.ConversationID) || !validRevision(b.Revision) || b.LimitUSD < 0 || b.LimitTokens < 0 || (b.LimitUSD == 0 && b.LimitTokens == 0) {
				return bad("invalid budget")
			}
			if b.Scope == "conversation" {
				if _, err := getConversation(b.ConversationID); err != nil {
					return err
				}
			}
		case strings.HasPrefix(row.Key, "approval/"):
			var a Approval
			if json.Unmarshal(row.Value, &a) != nil || !validID(a.ID) || row.Key != approvalKey(a.ID) || !validRevision(a.Revision) || a.Created.IsZero() || a.Tool == "" || a.CallID == "" || !validID(a.RunID) || a.ArgsHash != argsHash(a.Args) {
				return bad("invalid approval")
			}
			if _, err := getConversation(a.ConversationID); err != nil {
				return err
			}
			if _, ok := snap.Get(approvalConversationKey(a.ConversationID, a.ID)); !ok {
				return bad("approval without conversation index")
			}
			switch a.State {
			case approvalPending:
				if a.Decided != nil || a.Actor != "" {
					return bad("pending approval with decision")
				}
				if chain, ok := chains[a.ConversationID]; ok && chain.RunID == a.RunID {
					break
				}
				run, ok := runs[a.ConversationID]
				if !ok || run.ID != a.RunID || run.Phase != "tools" {
					return bad("pending approval without parked run")
				}
			case approvalAllowed, approvalDenied:
				if a.Decided == nil || a.Actor == "" || (a.Scope != "call" && a.Scope != "tool") {
					return bad("decided approval without provenance")
				}
			case approvalExpired:
			default:
				return bad("unknown approval state")
			}
		case strings.HasPrefix(row.Key, "approval-conversation/"):
			var id string
			if json.Unmarshal(row.Value, &id) != nil {
				return bad("invalid approval index")
			}
			a, ok, err := read[Approval](snap, approvalKey(id))
			if err != nil || !ok || row.Key != approvalConversationKey(a.ConversationID, a.ID) {
				return bad("approval index mismatch")
			}
		case strings.HasPrefix(row.Key, "task/"):
			var t Task
			if json.Unmarshal(row.Value, &t) != nil || !validID(t.ID) || row.Key != taskKey(t.ID) || t.Kind == "" || t.Version < 1 || !validRevision(t.Revision) || t.Created.IsZero() {
				return bad("invalid task")
			}
			if _, err := getConversation(t.ConversationID); err != nil {
				return err
			}
			if _, ok := snap.Get(taskConversationKey(t.ConversationID, t.ID)); !ok {
				return bad("task without conversation index")
			}
			for _, id := range t.Linked {
				if _, ok := snap.Get(taskKey(id)); !ok || id == t.ID {
					return bad("task links a missing task")
				}
			}
			if t.WaitApproval != "" && t.State != "waiting" {
				return bad("approval wait outside waiting state")
			}
			if t.Effect != "" && (t.Effect != effectStarted || (t.State != "running" && t.State != "waiting")) {
				return bad("task effect intent outside a running phase")
			}
			if t.Blocked != "" && t.State != "aborting" {
				return bad("blocked task outside cleanup")
			}
			if t.RetryAt != nil && (t.State == "terminal" || t.State == "waiting") {
				return bad("retry timer on settled or waiting task")
			}
			switch t.State {
			case "pending", "running":
				if t.Phase == "" || t.Outcome != "" || len(t.WaitOn) != 0 || t.WakeAt != nil {
					return bad("live task shape")
				}
			case "aborting":
				var held heldOutcome
				if t.Phase != "" || t.Outcome != "" || len(t.WaitOn) != 0 || t.WakeAt != nil || json.Unmarshal(t.Checkpoint, &held) != nil || (held.Outcome != "failed" && held.Outcome != "aborted") {
					return bad("aborting task shape")
				}
			case "waiting":
				kinds := 0
				for _, set := range []bool{len(t.WaitOn) > 0, t.WakeAt != nil, t.WaitApproval != ""} {
					if set {
						kinds++
					}
				}
				if t.Phase == "" || t.Outcome != "" || kinds != 1 {
					return bad("waiting task must have exactly one of dependencies, a timer, or an approval")
				}
				if t.WaitApproval != "" {
					if _, ok := snap.Get(approvalKey(t.WaitApproval)); !ok {
						return bad("task waits on a missing approval")
					}
				}
				if len(t.WaitOn) > 0 && t.WaitPolicy != "all" && t.WaitPolicy != "fail_fast" {
					return bad("waiting task policy")
				}
				for _, dep := range t.WaitOn {
					if dep == t.ID {
						return bad("task waits on itself")
					}
					if _, ok := snap.Get(taskKey(dep)); !ok {
						return bad("task waits on missing task")
					}
				}
			case "terminal":
				if t.Phase != "" || len(t.Checkpoint) != 0 || len(t.WaitOn) != 0 || t.WakeAt != nil || (t.Outcome != "completed" && t.Outcome != "failed" && t.Outcome != "aborted") {
					return bad("terminal task shape")
				}
			default:
				return bad("unknown task state")
			}
			// Ownership forms a forest: the owner exists, lives in the same
			// conversation, indexes this child, and the chain terminates.
			seen := map[string]bool{t.ID: true}
			for owner := t.Owner; owner != ""; {
				if seen[owner] {
					return bad("task ownership cycle")
				}
				seen[owner] = true
				o, ok, err := read[Task](snap, taskKey(owner))
				if err != nil || !ok || o.ConversationID != t.ConversationID {
					return bad("task owner reference")
				}
				owner = o.Owner
			}
			if t.Owner != "" {
				if _, ok := snap.Get(taskOwnerKey(t.Owner, t.ID)); !ok {
					return bad("task without owner index")
				}
			}
		case strings.HasPrefix(row.Key, "owned-conversation/"):
			var id string
			if json.Unmarshal(row.Value, &id) != nil || !strings.HasSuffix(row.Key, "/"+id) {
				return bad("owned conversation index")
			}
			c, err := getConversation(id)
			if err != nil {
				return err
			}
			if c.Owner == nil || row.Key != ownedConversationKey(c.Owner.ID, c.ID) {
				return bad("owned conversation without owner")
			}
		case strings.HasPrefix(row.Key, "owned-key/"):
			var id string
			if json.Unmarshal(row.Value, &id) != nil || !validDigest(strings.TrimPrefix(row.Key, "owned-key/")) {
				return bad("owned key reference")
			}
			c, err := getConversation(id)
			if err != nil {
				return err
			}
			if c.Owner == nil {
				return bad("owned key without owner")
			}
		case strings.HasPrefix(row.Key, "doc/"), strings.HasPrefix(row.Key, "doc-history/"):
			var d Document
			if json.Unmarshal(row.Value, &d) != nil || d.Kind == "" || d.Version < 1 || !validRevision(d.Revision) || d.Sequence == 0 || (!d.Deleted && !json.Valid(d.Value)) || (d.Deleted && len(d.Value) != 0 && string(d.Value) != "null") {
				return bad("invalid document")
			}
			if d.ConversationID != "" {
				c, err := getConversation(d.ConversationID)
				if err != nil {
					return err
				}
				if d.EntrySequence > c.EntrySequence || d.Revision > c.Revision {
					return bad("document outside conversation state")
				}
			}
			switch {
			case strings.HasPrefix(row.Key, "doc/"):
				if row.Key != documentKey(d.Kind, d.ConversationID) {
					return bad("document key mismatch")
				}
			default:
				if row.Key != documentHistoryKey(d.Kind, d.ConversationID, d.Sequence) {
					return bad("document history key mismatch")
				}
				current, ok, err := read[Document](snap, documentKey(d.Kind, d.ConversationID))
				if err != nil || !ok || current.Sequence < d.Sequence {
					return bad("document history beyond current")
				}
			}
		case strings.HasPrefix(row.Key, "task-owner/"), strings.HasPrefix(row.Key, "task-conversation/"):
			var id string
			if json.Unmarshal(row.Value, &id) != nil || !strings.HasSuffix(row.Key, "/"+id) {
				return bad("task index reference")
			}
			t, ok, err := read[Task](snap, taskKey(id))
			if err != nil || !ok {
				return bad("task index without task")
			}
			if strings.HasPrefix(row.Key, "task-owner/") && row.Key != taskOwnerKey(t.Owner, t.ID) {
				return bad("task owner index mismatch")
			}
			if strings.HasPrefix(row.Key, "task-conversation/") && row.Key != taskConversationKey(t.ConversationID, t.ID) {
				return bad("task conversation index mismatch")
			}
		case strings.HasPrefix(row.Key, "entry/"):
			var e Entry
			if json.Unmarshal(row.Value, &e) != nil || entryIDs[e.ID] != row.Key {
				return bad("orphan entry")
			}
		case strings.HasPrefix(row.Key, "submission/"):
			var s Submission
			if json.Unmarshal(row.Value, &s) != nil || !validID(s.ID) || row.Key != "submission/"+s.ID || !validRevision(s.Revision) || s.Sequence == 0 || strings.TrimSpace(s.Actor) == "" || strings.TrimSpace(s.Content) == "" || !userEntries[s.ID] {
				return bad("invalid or orphan submission")
			}
			c, err := getConversation(s.ConversationID)
			if err != nil {
				return err
			}
			if s.Revision > c.Revision {
				return bad("submission after conversation revision")
			}
			_, ok := queueSlots[s.ID]
			switch s.State {
			case "queued":
				if !ok || runSubmissions[s.ID] != "" {
					return bad("submission queue mismatch")
				}
				if s.Policy != "" && s.Policy != PolicySteer {
					return bad("unknown submission policy")
				}
				if s.Kind != "" && s.Kind != SubmissionWrite {
					return bad("unknown submission kind")
				}
			case "running":
				// Only the current run of a conversation is retained, so a running
				// submission must belong to it.
				if ok || runSubmissions[s.ID] == "" {
					return bad("running submission without active run or still queued")
				}
			case "answered", "failed", "aborted", "withdrawn", submissionWritten:
				if ok {
					return bad("settled submission still queued")
				}
			default:
				return bad("unknown submission state")
			}
			if s.RequestID != "" {
				// Retention may remove the deduplication record of a settled
				// submission; a live one must still have it.
				d, ok, err := read[Submission](snap, hashedKey("dedup/submit/", c.ID, s.Actor, s.RequestID))
				settled := s.State != "queued" && s.State != "running"
				if err != nil || (!ok && !settled) || (ok && d.ID == s.ID && !sameAdmission(d, s)) || (ok && d.ID != s.ID && !settled) {
					return bad("missing or mismatched deduplication")
				}
			}
			report.Submissions++
		case strings.HasPrefix(row.Key, "queue/"):
			var id string
			if json.Unmarshal(row.Value, &id) != nil {
				return bad("invalid queue reference")
			}
			s, err := getSubmission(id)
			if err != nil {
				return err
			}
			if !strings.HasPrefix(row.Key, "queue/"+s.ConversationID+"/") {
				return bad("queue key mismatch")
			}
		case strings.HasPrefix(row.Key, "dedup/submit/"):
			var d Submission
			if json.Unmarshal(row.Value, &d) != nil || d.RequestID == "" || row.Key != hashedKey("dedup/submit/", d.ConversationID, d.Actor, d.RequestID) {
				return bad("invalid deduplication key")
			}
			s, err := getSubmission(d.ID)
			if err != nil {
				return err
			}
			if !sameAdmission(d, s) {
				return bad("deduplication payload mismatch")
			}
		case strings.HasPrefix(row.Key, "root/") || strings.HasPrefix(row.Key, "import/session/"):
			prefix := "root/"
			if strings.HasPrefix(row.Key, "import/session/") {
				prefix = "import/session/"
			}
			if !validDigest(strings.TrimPrefix(row.Key, prefix)) {
				return bad("invalid conversation reference key")
			}
			var id string
			if json.Unmarshal(row.Value, &id) != nil {
				return bad("invalid conversation reference")
			}
			if _, err := getConversation(id); err != nil {
				return err
			}
			if strings.HasPrefix(row.Key, "import/session/") {
				info, ok, err := read[SessionImport](snap, "legacy/session/"+id)
				if err != nil || !ok || row.Key != "import/session/"+info.Digest {
					return bad("import reference mismatch")
				}
			}
		case strings.HasPrefix(row.Key, "legacy/session/"):
			id := strings.TrimPrefix(row.Key, "legacy/session/")
			c, err := getConversation(id)
			if err != nil {
				return err
			}
			var info SessionImport
			if json.Unmarshal(row.Value, &info) != nil || info.Rows < 1 || uint64(info.Rows) > c.EntrySequence || !validDigest(info.Digest) || info.ExecutionCheckpoints != "unavailable" {
				return bad("invalid import provenance")
			}
			var target string
			b, ok := snap.Get("import/session/" + info.Digest)
			if !ok || json.Unmarshal(b, &target) != nil || target != id {
				return bad("missing import reference")
			}
			// Source IDs may be absent and repaired only in the export projection.
			if info.Meta.ID != "" && info.Meta.ID != id {
				return bad("import identity mismatch")
			}
		default:
			return bad("unsupported record namespace")
		}
		return nil
	})
	if err != nil {
		return Integrity{}, err
	}
	report.Valid = true
	return report, nil
}

// sameAdmission compares the immutable admission of two submission records.
// The deduplication record keeps the admitted state, the submission advances.
func sameAdmission(a, b Submission) bool {
	a.State, b.State = "", ""
	return a == b
}
