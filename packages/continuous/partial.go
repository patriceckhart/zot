package continuous

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Partial is the streamed output of an in-flight model attempt. It is
// committed in batches so attached clients can render live text and so a
// crash retains what was streamed as part of the aborted attempt. It is not
// model context: the next request never includes partial output, and the
// record is removed when the attempt commits its final entry.
type Partial struct {
	ConversationID string    `json:"conversation_id"`
	RunID          string    `json:"run_id"`
	Turn           int       `json:"turn"`
	Attempt        int       `json:"attempt"`
	Text           string    `json:"text"`
	Updated        time.Time `json:"updated"`
	// Final marks the record as the retained output of an attempt that did
	// not complete. Live records are not final.
	Final bool `json:"final,omitempty"`
}

func partialKey(conversationID string) string { return "partial/" + conversationID }

// partialBatcher accumulates text deltas and commits them at most every
// interval. Commits of the partial record are not coordinated with run
// commits: they write only their own key, and any conflict is retried on a
// fresh snapshot so a run commit never fails because of a partial flush.
type partialBatcher struct {
	r        *Runtime
	run      Run
	interval time.Duration
	mu       sync.Mutex
	buf      strings.Builder
	dirty    bool
	stop     chan struct{}
	done     chan struct{}
}

// PartialFlushInterval bounds the display delay of streamed text for
// attached clients. It is a configured delay, not a durability promise.
const PartialFlushInterval = 100 * time.Millisecond

func (s *Service) startPartial(ctx context.Context, run Run) *partialBatcher {
	b := &partialBatcher{r: s.r, run: run, interval: s.opts.PartialFlushInterval, stop: make(chan struct{}), done: make(chan struct{})}
	if b.interval <= 0 {
		b.interval = PartialFlushInterval
	}
	go b.loop(ctx)
	return b
}

func (b *partialBatcher) add(delta string) {
	b.mu.Lock()
	b.buf.WriteString(delta)
	b.dirty = true
	b.mu.Unlock()
}

func (b *partialBatcher) loop(ctx context.Context) {
	defer close(b.done)
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			b.flush(ctx, false)
		}
	}
}

// flush commits the accumulated text when it changed. final retains the
// record for recovery instead of leaving it live.
func (b *partialBatcher) flush(ctx context.Context, final bool) {
	b.mu.Lock()
	if !b.dirty && !final {
		b.mu.Unlock()
		return
	}
	text := b.buf.String()
	b.dirty = false
	b.mu.Unlock()
	if text == "" && !final {
		return
	}
	p := Partial{ConversationID: b.run.ConversationID, RunID: b.run.ID, Turn: b.run.Turn, Attempt: b.run.Attempt, Text: text, Updated: time.Now().UTC(), Final: final}
	for attempt := 0; attempt < 8; attempt++ {
		snap, err := b.r.store.Snapshot(ctx)
		if err != nil {
			return
		}
		if err := b.r.commit(ctx, snap, "run.partial", record(partialKey(p.ConversationID), p)); err == nil || ctx.Err() != nil {
			return
		}
	}
}

// finish stops the batcher. When the attempt completed, the partial record
// is deleted in the caller's final commit through clearOp; when it did not,
// the text is retained as final for inspection.
func (b *partialBatcher) finish(ctx context.Context, completed bool) {
	close(b.stop)
	<-b.done
	if !completed {
		b.flush(context.WithoutCancel(ctx), true)
	}
}

// partialClearOp deletes the live partial record. Included in the commit of
// the attempt's final entry so no window exists where both are visible.
func partialClearOp(snap storage.Snapshot, conversationID string) []storage.Operation {
	if _, ok := snap.Get(partialKey(conversationID)); !ok {
		return nil
	}
	return []storage.Operation{{Key: partialKey(conversationID), Delete: true}}
}

// Partial returns the live or retained partial output of a conversation.
func (r *Runtime) Partial(ctx context.Context, conversationID string) (Partial, bool, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Partial{}, false, err
	}
	return read[Partial](snap, partialKey(conversationID))
}

// PartialFromCommit extracts a partial record a commit wrote, when any.
func PartialFromCommit(cm storage.Commit, conversationID string) (Partial, bool) {
	for _, op := range cm.Operations {
		if op.Key == partialKey(conversationID) && !op.Delete {
			var p Partial
			if json.Unmarshal(op.Value, &p) == nil {
				return p, true
			}
		}
	}
	return Partial{}, false
}
