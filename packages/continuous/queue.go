package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Queue operations on submissions that no run has claimed yet. Both are
// audited commits; neither touches a running or settled submission.

var ErrNotQueued = errors.New("continuous submission is not queued")

// Withdraw removes a queued submission. Its user entry stays in history and
// the submission settles with state withdrawn, so a request-ID retry returns
// the withdrawn record instead of admitting again.
func (r *Runtime) Withdraw(ctx context.Context, submissionID, actor string) (Submission, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Submission{}, err
		}
		s, ok, err := read[Submission](snap, "submission/"+submissionID)
		if err != nil {
			return Submission{}, err
		}
		if !ok {
			return Submission{}, ErrNotFound
		}
		if s.State != "queued" {
			return s, ErrNotQueued
		}
		// After a reorder the slot holding this submission may not be its
		// own sequence; find the slot that references it.
		slot := ""
		rows, err := snap.Page("queue/"+s.ConversationID+"/", "", 1000)
		if err != nil {
			return Submission{}, err
		}
		for _, row := range rows {
			var id string
			if json.Unmarshal(row.Value, &id) == nil && id == s.ID {
				slot = row.Key
			}
		}
		if slot == "" {
			return s, fmt.Errorf("%w: queued submission without a slot", storage.ErrCorrupt)
		}
		s.State = "withdrawn"
		ops := []storage.Operation{record("submission/"+s.ID, s), {Key: slot, Delete: true}}
		err = r.commit(ctx, snap, "queue.withdraw:"+actor, ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return s, err
	}
}

// Reorder moves a queued submission to the front of its conversation's
// queue. Queue order is the sequence stored in the queue key; the moved
// submission takes the position just before the current head by renumbering
// the queue keys, which is why the submission's own Sequence is unchanged and
// a Reorder is visible to watchers as one commit.
func (r *Runtime) Reorder(ctx context.Context, submissionID, actor string) ([]Submission, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		s, ok, err := read[Submission](snap, "submission/"+submissionID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrNotFound
		}
		if s.State != "queued" {
			return nil, ErrNotQueued
		}
		rows, err := snap.Page("queue/"+s.ConversationID+"/", "", 1000)
		if err != nil {
			return nil, err
		}
		var order []string
		var keys []string
		for _, row := range rows {
			var id string
			if json.Unmarshal(row.Value, &id) != nil {
				return nil, storage.ErrCorrupt
			}
			if id != s.ID {
				order = append(order, id)
			}
			keys = append(keys, row.Key)
		}
		order = append([]string{s.ID}, order...)
		if len(order) != len(keys) {
			return nil, fmt.Errorf("%w: submission missing from its queue", storage.ErrCorrupt)
		}
		// Rewrite the existing key slots in the new order.
		var ops []storage.Operation
		var result []Submission
		for i, key := range keys {
			ops = append(ops, record(key, order[i]))
			sub, _, err := read[Submission](snap, "submission/"+order[i])
			if err != nil {
				return nil, err
			}
			result = append(result, sub)
		}
		err = r.commit(ctx, snap, "queue.reorder:"+actor, ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return result, err
	}
}
