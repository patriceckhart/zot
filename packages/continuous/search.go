package continuous

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var errStop = errors.New("stop")

// SearchQuery selects entries from a conversation's full history, including
// entries before resets and compactions and inherited fork ancestry. It is a
// sequential scan over committed entries: no index is built or required.
type SearchQuery struct {
	ConversationID string `json:"conversation_id"`
	// Text, when set, matches case-insensitively against entry content.
	Text string `json:"text,omitempty"`
	// Types restricts entry types (user, assistant, tool_result, steer,
	// attempt, reset, compaction, legacy_*). Empty means all.
	Types []string `json:"types,omitempty"`
	// After and Before bound the entry time. Entries without a recorded
	// time match only when both bounds are zero.
	After  time.Time `json:"after,omitzero"`
	Before time.Time `json:"before,omitzero"`
	// Ancestry includes entries inherited from fork parents. Default false
	// returns only the conversation's own entries.
	Ancestry bool `json:"ancestry,omitempty"`
	// Cursor continues a previous page: the owner conversation and sequence
	// of the last returned entry. Zero starts from the beginning.
	Cursor SearchCursor `json:"cursor,omitzero"`
	// Limit is 1..1000. Zero means 100.
	Limit int `json:"limit,omitempty"`
}

// SearchCursor addresses a position in a conversation's history.
type SearchCursor struct {
	Owner    string `json:"owner,omitempty"`
	Sequence uint64 `json:"sequence,omitempty"`
}

// SearchHit is one matching entry with its position.
type SearchHit struct {
	Owner    string `json:"owner"`
	Sequence uint64 `json:"sequence"`
	Entry    Entry  `json:"entry"`
}

// SearchResult is one page of hits.
type SearchResult struct {
	Revision uint64       `json:"revision"`
	Hits     []SearchHit  `json:"hits"`
	Next     SearchCursor `json:"next,omitzero"`
	More     bool         `json:"more"`
}

// Search scans one committed snapshot. Hits are in history order, oldest
// first. The scan stops after Limit hits and returns the cursor to resume.
func (r *Runtime) Search(ctx context.Context, q SearchQuery) (SearchResult, error) {
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 1000 {
		return SearchResult{}, fmt.Errorf("search limit must be between 1 and 1000")
	}
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return SearchResult{}, err
	}
	if _, err := conversation(snap, q.ConversationID); err != nil {
		return SearchResult{}, err
	}
	types := map[string]bool{}
	for _, t := range q.Types {
		types[t] = true
	}
	needle := strings.ToLower(q.Text)
	result := SearchResult{Revision: snap.Revision(), Hits: []SearchHit{}}
	started := q.Cursor.Owner == ""
	err = History(ctx, snap, q.ConversationID, 0, func(owner string, seq uint64, e Entry) error {
		if !started {
			if owner == q.Cursor.Owner && seq == q.Cursor.Sequence {
				started = true
			}
			return nil
		}
		if !q.Ancestry && owner != q.ConversationID {
			return nil
		}
		if len(types) > 0 && !types[e.Type] || len(types) == 0 && e.Type == entryContext {
			// Context entries are request shape, not conversation
			// content; they are searched only when asked for by type.
			return nil
		}
		if !q.After.IsZero() || !q.Before.IsZero() {
			if e.Time.IsZero() {
				return nil
			}
			if !q.After.IsZero() && e.Time.Before(q.After) {
				return nil
			}
			if !q.Before.IsZero() && !e.Time.Before(q.Before) {
				return nil
			}
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.Content), needle) {
			return nil
		}
		if len(result.Hits) >= q.Limit {
			result.More = true
			return errStop
		}
		result.Hits = append(result.Hits, SearchHit{Owner: owner, Sequence: seq, Entry: e})
		result.Next = SearchCursor{Owner: owner, Sequence: seq}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return SearchResult{}, err
	}
	return result, nil
}
