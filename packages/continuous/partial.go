package continuous

import (
	"context"
	"encoding/json"
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
	// Truncated is set when Text reached MaxPartialBytes and later output
	// was not persisted.
	Truncated bool `json:"truncated,omitempty"`
}

// MaxPartialBytes bounds a persisted partial record. Output beyond it is
// streamed to live observers but not committed.
const MaxPartialBytes = 256 << 10

func partialKey(conversationID string) string { return "partial/" + conversationID }

// PartialFlushInterval bounds the display delay of streamed text for
// attached clients. It is a configured delay, not a durability promise.
const PartialFlushInterval = 100 * time.Millisecond

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
