package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// livePartial captures RAM before taking the storage snapshot. The snapshot
// fences the attempt and provides the revision barrier for its preview. All
// commits through that barrier must be delivered before the preview.
func (r *Runtime) livePartial(ctx context.Context, id string) (Partial, bool, storage.Snapshot, error) {
	var rec Partial
	value, loaded := r.livePartials.Load(id)
	var p *taskPartial
	if loaded {
		p = value.(*taskPartial)
		p.mu.Lock()
		rec = p.base
		rec.Text, rec.Truncated = p.buf.String(), p.trunc
		rec.Reasoning, rec.ReasoningBytes = p.reasoning, p.reasoningBytes
		p.mu.Unlock()
	}
	snap, err := r.store.Snapshot(ctx)
	if err != nil || p == nil {
		return Partial{}, false, snap, err
	}
	raw, ok, err := storage.Get(ctx, snap, taskKey(p.taskID))
	if err != nil || !ok {
		return Partial{}, false, snap, err
	}
	var current Task
	if json.Unmarshal(raw, &current) != nil {
		return Partial{}, false, snap, storage.ErrCorrupt
	}
	active, present := r.livePartials.Load(id)
	valid := current.Invocation == p.invocation && current.State != "terminal" && present && active == p
	return rec, valid, snap, nil
}

// watchLive adds disposable RAM previews to the durable watch. Previews have
// no cursor and are never replayed from storage. Each new watch samples RAM
// again, so joining and reconnecting do not wait for the next checkpoint.
func (r *Runtime) watchLive(ctx context.Context, id string, after uint64, commit func(storage.Commit) error, preview func(Partial) error) error {
	cursor := after
	var previous Partial
	nextPreview := time.Time{}
	prefixes := conversationWatchPrefixes(id)
	for {
		rec, live, snap, err := r.livePartial(ctx, id)
		if err != nil {
			return err
		}
		// Do not scan past the captured barrier. A final response committed
		// after capture must follow its preview, never precede it.
		for cursor < snap.Revision() {
			commits, err := r.Scan(ctx, cursor, 100)
			if err != nil {
				return err
			}
			if len(commits) == 0 {
				return storage.ErrCursor
			}
			for _, cm := range commits {
				if cm.Revision > snap.Revision() {
					break
				}
				cursor = cm.Revision
				if commitTouches(cm, prefixes) {
					if err := commit(cm); err != nil {
						return err
					}
				}
			}
		}
		now := time.Now()
		if !now.Before(nextPreview) {
			if live && rec != previous {
				previous = rec
				rec.Updated = now.UTC()
				rec.PreviewAfter = snap.Revision()
				if err := preview(rec); err != nil {
					return err
				}
			}
			nextPreview = now.Add(PartialFlushInterval)
		}
		waitCtx, cancel := context.WithDeadline(ctx, nextPreview)
		err = r.Wait(waitCtx, cursor)
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}
