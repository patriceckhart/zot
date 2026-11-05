package continuous

import (
	"context"
	"encoding/json"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Partial is the streamed output of an in-flight model attempt. Attached
// clients can preview it from RAM between persisted checkpoints. A crash
// retains only the last checkpoint of the aborted attempt. It is not
// model context: the next request never includes partial output, and the
// record is removed when the attempt commits its final entry.
type Partial struct {
	ConversationID string `json:"conversation_id"`
	RunID          string `json:"run_id"`
	Turn           int    `json:"turn"`
	Attempt        int    `json:"attempt"`
	Text           string `json:"text"`
	// PreviewAfter is the storage barrier preceding a RAM preview. It is
	// zero in persisted records and never advances a durable watch cursor.
	// Views refreshed beyond this revision must ignore the older preview.
	PreviewAfter uint64 `json:"preview_after,omitempty"`
	// Reasoning is a bounded readable suffix, separate from model context.
	Reasoning string `json:"reasoning,omitempty"`
	// ReasoningBytes counts all reasoning bytes, including discarded prefixes.
	// Attached clients use it to avoid replaying overlapping suffixes.
	ReasoningBytes int64     `json:"reasoning_bytes,omitempty"`
	Updated        time.Time `json:"updated"`
	// Final marks the record as the retained output of an attempt that did
	// not complete. Live records are not final.
	Final bool `json:"final,omitempty"`
	// Truncated is set when Text reached MaxPartialBytes and later output
	// was not persisted.
	Truncated bool `json:"truncated,omitempty"`
}

// MaxPartialBytes bounds answer text in a persisted partial record.
// Output beyond it is streamed to live observers but not committed.
const MaxPartialBytes = 256 << 10

// MaxPartialReasoningBytes bounds the live reasoning suffix independently.
const MaxPartialReasoningBytes = 32 << 10

func partialKey(conversationID string) string { return "partial/" + conversationID }

// PartialFlushInterval bounds the display delay of streamed text for
// attached clients. It is a configured delay, not a durability promise.
const PartialFlushInterval = 100 * time.Millisecond

// PartialPersistInterval is the default delay between partial checkpoints.
// Final responses are committed immediately, independently of this interval.
const PartialPersistInterval = time.Second

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
