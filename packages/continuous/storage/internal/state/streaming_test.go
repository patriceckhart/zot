package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestStreamingHistoryNotRetained(t *testing.T) {
	const count = 400
	ctx := context.Background()
	var persisted storage.Commit
	scans := 0
	s, err := NewStreaming(storage.Capabilities{Durability: storage.Process}, func(visit func(storage.Commit) error) error {
		for i := uint64(1); i <= count; i++ {
			c := storage.Commit{Schema: 1, Revision: i, Epoch: 3, Operations: []storage.Operation{{Key: "replaced", Value: json.RawMessage(fmt.Sprint(i))}}}
			if err := visit(c); err != nil {
				return err
			}
		}
		return nil
	}, func(ctx context.Context, after uint64, limit int) ([]storage.Commit, error) {
		scans++
		return []storage.Commit{cloneCommit(persisted)}, nil
	}, func(c storage.Commit) error { persisted = cloneCommit(c); return nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.history) != 0 || s.revision != count+1 || s.Epoch() != 4 {
		t.Fatalf("retained history or wrong acquisition: history=%d revision=%d epoch=%d", len(s.history), s.revision, s.Epoch())
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, ok := snap.Get("replaced")
	if !ok || string(b) != "400" || snap.Revision() != count+1 {
		t.Fatalf("projection: %s %v", b, ok)
	}
	c, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "replaced", Delete: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.history) != 0 || c.Revision != count+2 {
		t.Fatal("live commit retained in history")
	}
	rows, err := s.Scan(ctx, count+1, 1)
	if err != nil || len(rows) != 1 || rows[0].Revision != c.Revision || scans != 1 {
		t.Fatalf("scan callback: %+v %v", rows, err)
	}
	if _, err := s.Scan(ctx, c.Revision+1, 1); !errors.Is(err, storage.ErrCursor) || scans != 1 {
		t.Fatal("invalid cursor reached scanner")
	}
	if _, err := s.Scan(ctx, c.Revision, 0); err == nil || scans != 1 {
		t.Fatal("invalid limit reached scanner")
	}
	if err := s.Wait(ctx, c.Revision-1); err != nil {
		t.Fatal(err)
	}
	if b, ok := snap.Get("replaced"); !ok || string(b) != "400" {
		t.Fatal("historical snapshot changed")
	}
	latest, _ := s.Snapshot(ctx)
	if _, ok := latest.Get("replaced"); ok {
		t.Fatal("deletion not projected")
	}
}

func TestStreamingFailures(t *testing.T) {
	failure := errors.New("synthetic backend error")
	replay := func(func(storage.Commit) error) error { return nil }
	scan := func(context.Context, uint64, int) ([]storage.Commit, error) { return nil, failure }
	persist := func(storage.Commit) error { return nil }
	for _, phase := range []string{"missing-replay", "missing-scan", "missing-persist", "replay", "invalid-replay", "acquisition"} {
		t.Run(phase, func(t *testing.T) {
			r, p, q := replay, persist, scan
			switch phase {
			case "missing-replay":
				r = nil
			case "missing-scan":
				q = nil
			case "missing-persist":
				p = nil
			case "replay":
				r = func(func(storage.Commit) error) error { return failure }
			case "invalid-replay":
				r = func(visit func(storage.Commit) error) error {
					return visit(storage.Commit{Schema: 1, Revision: 2, Epoch: 1})
				}
			case "acquisition":
				p = func(storage.Commit) error { return failure }
			}
			s, err := NewStreaming(storage.Capabilities{}, r, q, p, nil)
			if err == nil || s != nil {
				t.Fatalf("accepted failure: %v", err)
			}
		})
	}
	calls := 0
	s, err := NewStreaming(storage.Capabilities{}, replay, scan, func(storage.Commit) error {
		calls++
		if calls > 1 {
			return failure
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if rows, err := s.Scan(context.Background(), 0, 1); !errors.Is(err, failure) || len(rows) != 0 {
		t.Fatalf("scan failure: %v %v", rows, err)
	}
	_, err = s.Commit(context.Background(), storage.Mutation{ExpectedRevision: 1, Epoch: s.Epoch()})
	if !errors.Is(err, failure) || s.revision != 1 || len(s.history) != 0 {
		t.Fatal("failed persistence applied state")
	}
	if err := s.Wait(context.Background(), 1); !errors.Is(err, storage.ErrClosed) {
		t.Fatal("poisoned writer not closed")
	}
}

func TestRevisionExhaustion(t *testing.T) {
	s, err := New(storage.Capabilities{}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.revision = ^uint64(0)
	if _, err := s.Commit(context.Background(), storage.Mutation{ExpectedRevision: s.revision, Epoch: s.Epoch()}); err == nil {
		t.Fatal("wrapped store revision")
	}
}
