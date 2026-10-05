package continuous

import (
	"context"
	"encoding/json"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

type Status struct {
	Revision         uint64               `json:"revision"`
	WriterEpoch      uint64               `json:"writer_epoch"`
	Capabilities     storage.Capabilities `json:"capabilities"`
	ExecutionEnabled bool                 `json:"execution_enabled"`
}

func (r *Runtime) Status(ctx context.Context) (Status, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{Revision: snap.Revision(), WriterEpoch: r.store.Epoch(), Capabilities: r.store.Capabilities()}, nil
}

// Conversations returns a page at one committed revision. after is the last
// returned conversation ID. Empty starts the first page. Limits are bounded by
// the backend. A later call obtains a new snapshot, not a historical read lease.
func (r *Runtime) Conversations(ctx context.Context, after string, limit int) ([]Conversation, uint64, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return nil, 0, err
	}
	cursor := ""
	if after != "" {
		cursor = "conversation/" + after
	}
	rows, err := snap.Page("conversation/", cursor, limit)
	if err != nil {
		return nil, 0, err
	}
	out := make([]Conversation, 0, len(rows))
	for _, row := range rows {
		var c Conversation
		if err := json.Unmarshal(row.Value, &c); err != nil || row.Key != "conversation/"+c.ID {
			return nil, 0, storage.ErrCorrupt
		}
		out = append(out, c)
	}
	return out, snap.Revision(), nil
}

// Submission reads a committed submission record.
func (r *Runtime) Submission(ctx context.Context, id string) (Submission, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Submission{}, err
	}
	s, ok, err := read[Submission](snap, "submission/"+id)
	if err != nil {
		return Submission{}, err
	}
	if !ok {
		return Submission{}, ErrNotFound
	}
	return s, nil
}

// WaitSubmission blocks until the submission leaves the queued and running
// states or ctx ends. Cancelling the wait never cancels the work. Nothing
// executes work unless a Service steps the conversation.
func (r *Runtime) WaitSubmission(ctx context.Context, id string) (Submission, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Submission{}, err
		}
		s, ok, err := read[Submission](snap, "submission/"+id)
		if err != nil {
			return Submission{}, err
		}
		if !ok {
			return Submission{}, ErrNotFound
		}
		if s.State != "queued" && s.State != "running" {
			return s, nil
		}
		if err := r.store.Wait(ctx, snap.Revision()); err != nil {
			return s, err
		}
	}
}

func (r *Runtime) SessionImport(ctx context.Context, id string) (SessionImport, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return SessionImport{}, err
	}
	info, ok, err := read[SessionImport](snap, "legacy/session/"+id)
	if err != nil {
		return SessionImport{}, err
	}
	if !ok {
		return SessionImport{}, ErrNotFound
	}
	return info, nil
}
