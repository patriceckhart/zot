package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// RetentionPolicy bounds how long settled, derived, or auxiliary records are
// kept. Transcript entries, runs, usage rows, and task records are never
// collected: they are the history. Zero durations keep a class forever.
type RetentionPolicy struct {
	// Dedup removes request-ID deduplication records older than this. A
	// retry after expiry is admitted as new work, so choose it longer than
	// any client's retry window.
	Dedup time.Duration
	// Approvals removes decided or expired approvals older than this.
	// Pending approvals are never collected.
	Approvals time.Duration
	// Partials removes retained final partial output older than this.
	Partials time.Duration
	// Memos removes memos of runs and tasks that settled longer ago than
	// this. Memos of live runs and tasks are never collected.
	Memos time.Duration
	// DocumentHistory keeps at most this many historical values per
	// document. Zero keeps all. Documents with as_of fork semantics keep at
	// least the values any fork point references.
	DocumentHistory int
}

// RetentionReport counts what a sweep removed.
type RetentionReport struct {
	Revision  uint64 `json:"revision"`
	Dedup     int    `json:"dedup"`
	Approvals int    `json:"approvals"`
	Partials  int    `json:"partials"`
	Memos     int    `json:"memos"`
	Documents int    `json:"document_versions"`
	DryRun    bool   `json:"dry_run,omitempty"`
}

// Retain applies the policy at one committed snapshot. Deletions are
// committed in batches; a conflict restarts the batch on a fresh snapshot.
// DryRun reports without deleting. Retention is garbage collection of
// auxiliary records, not sensitive-data erasure: journal segments keep the
// bytes until the journal itself is compacted or rewritten.
func (r *Runtime) Retain(ctx context.Context, policy RetentionPolicy, dryRun bool) (RetentionReport, error) {
	now := time.Now().UTC()
	report := RetentionReport{DryRun: dryRun}
	for attempt := 0; attempt < 16; attempt++ {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return report, err
		}
		report = RetentionReport{Revision: snap.Revision(), DryRun: dryRun}
		var ops []storage.Operation
		add := func(key string) { ops = append(ops, storage.Operation{Key: key, Delete: true}) }
		each := func(prefix string, visit func(row storage.Record) error) error {
			after := ""
			for {
				if err := ctx.Err(); err != nil {
					return err
				}
				page, err := snap.Page(prefix, after, 500)
				if err != nil {
					return err
				}
				if len(page) == 0 {
					return nil
				}
				for _, row := range page {
					after = row.Key
					if err := visit(row); err != nil {
						return err
					}
				}
			}
		}
		liveRuns := map[string]bool{}
		if err := each("run/", func(row storage.Record) error {
			var run Run
			if json.Unmarshal(row.Value, &run) == nil && run.Phase != "done" {
				liveRuns["run/"+run.ID] = true
			}
			return nil
		}); err != nil {
			return report, err
		}
		liveTasks := map[string]bool{}
		if err := each("task/", func(row storage.Record) error {
			var t Task
			if json.Unmarshal(row.Value, &t) == nil && t.State != "terminal" {
				liveTasks["task/"+t.ID] = true
			}
			return nil
		}); err != nil {
			return report, err
		}
		if policy.Dedup > 0 {
			if err := each("dedup/submit/", func(row storage.Record) error {
				var s Submission
				if json.Unmarshal(row.Value, &s) != nil {
					return nil
				}
				// A dedup record has no timestamp of its own; its user entry does.
				live, _, _ := read[Submission](snap, "submission/"+s.ID)
				if live.State == "queued" || live.State == "running" {
					return nil
				}
				c, err := conversation(snap, s.ConversationID)
				if err != nil {
					return nil
				}
				e, ok, _ := read[Entry](snap, entryKey(c.ID, entrySequenceOf(snap, c.ID, s.ID)))
				if ok && !e.Time.IsZero() && now.Sub(e.Time) > policy.Dedup {
					report.Dedup++
					add(row.Key)
				}
				return nil
			}); err != nil {
				return report, err
			}
		}
		if policy.Approvals > 0 {
			if err := each("approval/", func(row storage.Record) error {
				var a Approval
				if json.Unmarshal(row.Value, &a) != nil || a.State == approvalPending {
					return nil
				}
				at := a.Created
				if a.Decided != nil {
					at = *a.Decided
				}
				if now.Sub(at) > policy.Approvals {
					report.Approvals++
					add(row.Key)
					add(approvalConversationKey(a.ConversationID, a.ID))
				}
				return nil
			}); err != nil {
				return report, err
			}
		}
		if policy.Partials > 0 {
			if err := each("partial/", func(row storage.Record) error {
				var p Partial
				if json.Unmarshal(row.Value, &p) == nil && p.Final && now.Sub(p.Updated) > policy.Partials {
					report.Partials++
					add(row.Key)
				}
				return nil
			}); err != nil {
				return report, err
			}
		}
		if policy.Memos > 0 {
			if err := each("memo/", func(row storage.Record) error {
				var m Memo
				if json.Unmarshal(row.Value, &m) != nil || liveRuns[m.Scope] || liveTasks[m.Scope] || now.Sub(m.Created) <= policy.Memos {
					return nil
				}
				report.Memos++
				add(row.Key)
				return nil
			}); err != nil {
				return report, err
			}
		}
		if policy.DocumentHistory > 0 {
			// Group history rows per document, keep the newest N plus any
			// value a fork point references.
			type hist struct {
				keys []string
				docs []Document
			}
			groups := map[string]*hist{}
			if err := each("doc-history/", func(row storage.Record) error {
				var d Document
				if json.Unmarshal(row.Value, &d) != nil {
					return nil
				}
				group := row.Key[:strings.LastIndex(row.Key, "/")]
				g := groups[group]
				if g == nil {
					g = &hist{}
					groups[group] = g
				}
				g.keys = append(g.keys, row.Key)
				g.docs = append(g.docs, d)
				return nil
			}); err != nil {
				return report, err
			}
			forkPoints := map[string]map[uint64]bool{}
			if err := each("conversation/", func(row storage.Record) error {
				var c Conversation
				if json.Unmarshal(row.Value, &c) == nil && c.Parent != nil {
					if forkPoints[c.Parent.ConversationID] == nil {
						forkPoints[c.Parent.ConversationID] = map[uint64]bool{}
					}
					forkPoints[c.Parent.ConversationID][c.Parent.At] = true
				}
				return nil
			}); err != nil {
				return report, err
			}
			for _, g := range groups {
				excess := len(g.keys) - policy.DocumentHistory
				for i := 0; i < excess; i++ {
					d := g.docs[i]
					// Keep the newest value at or before any fork point.
					referenced := false
					for at := range forkPoints[d.ConversationID] {
						if d.EntrySequence <= at && (i+1 >= len(g.docs) || g.docs[i+1].EntrySequence > at) {
							referenced = true
						}
					}
					if referenced {
						continue
					}
					report.Documents++
					add(g.keys[i])
				}
			}
		}
		if dryRun || len(ops) == 0 {
			return report, nil
		}
		err = r.commit(ctx, snap, "retention", ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return report, err
	}
	return report, fmt.Errorf("retention sweep kept conflicting with writers; retry when the store is quieter")
}

// entrySequenceOf finds the admission entry of a submission by scanning the
// conversation's entries. Conversations are small enough for the sweep;
// an index would be premature.
func entrySequenceOf(snap storage.Snapshot, conversationID, submissionID string) uint64 {
	after := ""
	for {
		page, err := snap.Page("entry/"+conversationID+"/", after, 500)
		if err != nil || len(page) == 0 {
			return 0
		}
		for _, row := range page {
			after = row.Key
			var e Entry
			if json.Unmarshal(row.Value, &e) == nil && e.SubmissionID == submissionID && e.Type == "user" {
				var seq uint64
				fmt.Sscanf(row.Key[strings.LastIndex(row.Key, "/")+1:], "%d", &seq)
				return seq
			}
		}
	}
}
