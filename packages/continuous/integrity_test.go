package continuous

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
)

func TestIntegrityValid(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)
	report, err := r.CheckIntegrity(ctx)
	if err != nil || !report.Valid || report.Conversations != 0 {
		t.Fatalf("empty: %+v %v", report, err)
	}
	c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 105; i++ {
		if _, err := r.Submit(ctx, c.ID, "actor", "", "private input"); err != nil {
			t.Fatal(err)
		}
	}
	imported, err := r.ImportSession(ctx, strings.NewReader(`{"type":"meta","meta":{"id":"legacy"}}
{"type":"message","message":{"role":"user","content":[{"type":"text","text":"historical input"}]}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, imported.ID, "actor", "retry-key", "new input"); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Snapshot(ctx)
	report, err = r.CheckIntegrity(ctx)
	if err != nil || !report.Valid || report.Scope != "admission-state" || report.Revision != before.Revision() || report.Conversations != 2 || report.Submissions != 106 || report.Entries != 108 {
		t.Fatalf("report: %+v %v", report, err)
	}
	after, _ := r.Snapshot(ctx)
	if after.Revision() != before.Revision() {
		t.Fatal("check mutated state")
	}
}

func TestIntegrityCorruption(t *testing.T) {
	tests := []struct {
		name   string
		change func(storage.Snapshot, Conversation, Submission) []storage.Operation
	}{
		{"missing conversation", func(_ storage.Snapshot, c Conversation, _ Submission) []storage.Operation {
			return []storage.Operation{{Key: "conversation/" + c.ID, Delete: true}}
		}},
		{"missing entry", func(_ storage.Snapshot, c Conversation, _ Submission) []storage.Operation {
			return []storage.Operation{{Key: entryKey(c.ID, 1), Delete: true}}
		}},
		{"running submission without chain", func(_ storage.Snapshot, c Conversation, _ Submission) []storage.Operation {
			return []storage.Operation{{Key: chainKey(c.ID), Delete: true}}
		}},
		{"missing submission", func(_ storage.Snapshot, _ Conversation, s Submission) []storage.Operation {
			return []storage.Operation{{Key: "submission/" + s.ID, Delete: true}}
		}},
		{"missing dedup", func(_ storage.Snapshot, c Conversation, s Submission) []storage.Operation {
			return []storage.Operation{{Key: hashedKey("dedup/submit/", c.ID, s.Actor, s.RequestID), Delete: true}}
		}},
		{"wrong dedup content", func(_ storage.Snapshot, c Conversation, s Submission) []storage.Operation {
			key := hashedKey("dedup/submit/", c.ID, s.Actor, s.RequestID)
			s.Content = "different"
			return []storage.Operation{record(key, s)}
		}},
		{"wrong queue counter", func(_ storage.Snapshot, c Conversation, _ Submission) []storage.Operation {
			c.QueueSequence++
			return []storage.Operation{record("conversation/"+c.ID, c)}
		}},
		{"future revision", func(snap storage.Snapshot, c Conversation, _ Submission) []storage.Operation {
			c.Revision = snap.Revision() + 2
			return []storage.Operation{record("conversation/"+c.ID, c)}
		}},
		{"unlinked duplicate entry", func(snap storage.Snapshot, c Conversation, _ Submission) []storage.Operation {
			e, _, _ := read[Entry](snap, entryKey(c.ID, 1))
			return []storage.Operation{record("entry/absent/00000000000000000001", e)}
		}},
		{"wrong entry content", func(snap storage.Snapshot, c Conversation, _ Submission) []storage.Operation {
			e, _, _ := read[Entry](snap, entryKey(c.ID, 1))
			e.Content = "different"
			return []storage.Operation{record(entryKey(c.ID, 1), e)}
		}},
		{"wrong state", func(_ storage.Snapshot, _ Conversation, s Submission) []storage.Operation {
			s.State = "completed"
			return []storage.Operation{record("submission/"+s.ID, s)}
		}},
		{"unknown namespace", func(_ storage.Snapshot, _ Conversation, _ Submission) []storage.Operation {
			return []storage.Operation{record("private-secret/key", "private-secret-value")}
		}},
		{"null conversation", func(_ storage.Snapshot, c Conversation, _ Submission) []storage.Operation {
			return []storage.Operation{record("conversation/"+c.ID, nil)}
		}},
		{"dangling root", func(_ storage.Snapshot, _ Conversation, _ Submission) []storage.Operation {
			return []storage.Operation{record(hashedKey("root/", "second"), "missing")}
		}},
		{"orphan dedup", func(_ storage.Snapshot, _ Conversation, s Submission) []storage.Operation {
			s.Actor = "other"
			return []storage.Operation{record(hashedKey("dedup/submit/", s.ConversationID, s.Actor, s.RequestID), s)}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := newTestRuntime(t)
			ctx := context.Background()
			c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
			if err != nil {
				t.Fatal(err)
			}
			s, err := r.Submit(ctx, c.ID, "actor", "request", "private-secret-value")
			if err != nil {
				t.Fatal(err)
			}
			snap, _ := r.Snapshot(ctx)
			if err := r.commit(ctx, snap, "test", test.change(snap, c, s)...); err != nil {
				t.Fatal(err)
			}
			before, _ := r.Snapshot(ctx)
			report, err := r.CheckIntegrity(ctx)
			if !errors.Is(err, storage.ErrCorrupt) || report.Valid {
				t.Fatalf("accepted corruption: %+v %v", report, err)
			}
			if strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), s.ID) || strings.Contains(err.Error(), c.ID) {
				t.Fatalf("private data in error: %v", err)
			}
			after, _ := r.Snapshot(ctx)
			if after.Revision() != before.Revision() {
				t.Fatal("check repaired state")
			}
		})
	}
}

func TestIntegrityCancellationAndClosed(t *testing.T) {
	r := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.CheckIntegrity(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CheckIntegrity(context.Background()); !errors.Is(err, storage.ErrClosed) {
		t.Fatal(err)
	}
}

func TestIntegrityJournalReopen(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	store, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := New(store)
	c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, c.ID, "actor", "request", "input"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	r, _ = New(store)
	defer r.Close()
	report, err := r.CheckIntegrity(ctx)
	if err != nil || !report.Valid || report.Submissions != 1 {
		t.Fatalf("reopened: %+v %v", report, err)
	}
}

// Interleave a mutation after the detached snapshot is captured, without sleeps
// or a second goroutine. Every integrity read must stay on the captured state.
type integrityInterleaveStore struct {
	storage.Store
	afterSnapshot func(storage.Snapshot) error
}

func (s *integrityInterleaveStore) Snapshot(ctx context.Context) (storage.Snapshot, error) {
	snap, err := s.Store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if hook := s.afterSnapshot; hook != nil {
		s.afterSnapshot = nil
		if err := hook(snap); err != nil {
			return nil, err
		}
	}
	return snap, nil
}

func TestIntegritySnapshotConsistency(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)
	c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	store := r.store
	var captured uint64
	r.store = &integrityInterleaveStore{Store: store, afterSnapshot: func(snap storage.Snapshot) error {
		captured = snap.Revision()
		_, err := store.Commit(ctx, storage.Mutation{ExpectedRevision: captured, Epoch: store.Epoch(), Operations: []storage.Operation{{Key: "conversation/" + c.ID, Delete: true}}})
		return err
	}}
	report, err := r.CheckIntegrity(ctx)
	if err != nil || !report.Valid || report.Revision != captured || report.Conversations != 1 {
		t.Fatalf("mixed snapshots: %+v %v", report, err)
	}
	if _, err := r.CheckIntegrity(ctx); !errors.Is(err, storage.ErrCorrupt) {
		t.Fatalf("latest state should fail: %v", err)
	}
}

func TestIntegrityCancellationDuringScan(t *testing.T) {
	r := newTestRuntime(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.store = &integrityInterleaveStore{Store: r.store, afterSnapshot: func(storage.Snapshot) error {
		cancel()
		return nil
	}}
	if report, err := r.CheckIntegrity(ctx); !errors.Is(err, context.Canceled) || report.Valid {
		t.Fatalf("cancelled scan: %+v %v", report, err)
	}
}

func TestIntegrityImportCorruption(t *testing.T) {
	for _, kind := range []string{"provenance", "reference", "rows", "digest", "checkpoint", "identity", "legacy-row"} {
		t.Run(kind, func(t *testing.T) {
			r := newTestRuntime(t)
			ctx := context.Background()
			c, err := r.ImportSession(ctx, strings.NewReader(`{"type":"meta","meta":{"id":"legacy"}}`))
			if err != nil {
				t.Fatal(err)
			}
			snap, _ := r.Snapshot(ctx)
			info, _, _ := read[SessionImport](snap, "legacy/session/"+c.ID)
			var op storage.Operation
			switch kind {
			case "provenance":
				op = storage.Operation{Key: "legacy/session/" + c.ID, Delete: true}
			case "reference":
				op = storage.Operation{Key: "import/session/" + info.Digest, Delete: true}
			case "legacy-row":
				e, _, _ := read[Entry](snap, entryKey(c.ID, 1))
				e.LegacyRow = nil
				op = record(entryKey(c.ID, 1), e)
			default:
				switch kind {
				case "rows":
					info.Rows++
				case "digest":
					info.Digest = "invalid"
				case "checkpoint":
					info.ExecutionCheckpoints = "completed"
				case "identity":
					info.Meta.ID = "other"
				}
				op = record("legacy/session/"+c.ID, info)
			}
			if err := r.commit(ctx, snap, "test", op); err != nil {
				t.Fatal(err)
			}
			if report, err := r.CheckIntegrity(ctx); !errors.Is(err, storage.ErrCorrupt) || report.Valid {
				t.Fatalf("accepted invalid import: %+v %v", report, err)
			}
		})
	}
}
