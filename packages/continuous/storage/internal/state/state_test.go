package state

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestFailedBarrierFencesWriter(t *testing.T) {
	failure := errors.New("synthetic sync failure")
	calls, closes := 0, 0
	s, err := New(storage.Capabilities{Durability: storage.Process}, nil, func(storage.Commit) error {
		calls++
		if calls > 1 {
			return failure
		}
		return nil
	}, func() error { closes++; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	snap, _ := s.Snapshot(ctx)
	_, err = s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "test", Value: json.RawMessage(`1`)}}})
	if !errors.Is(err, failure) {
		t.Fatalf("barrier failure: %v", err)
	}
	if _, ok := snap.Get("test"); ok {
		t.Fatal("failed commit visible in prior snapshot")
	}
	if len(s.history) != 1 {
		t.Fatal("failed commit added to history")
	}
	if _, err := s.Snapshot(ctx); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("failed writer still readable: %v", err)
	}
	if _, err := s.Commit(ctx, storage.Mutation{}); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("failed writer still writable: %v", err)
	}
	if err := s.Wait(ctx, snap.Revision()); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("wait blocked: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if closes != 1 {
		t.Fatalf("cleanup calls: %d", closes)
	}
}

func TestInvalidHistory(t *testing.T) {
	for _, c := range []storage.Commit{
		{Schema: 2, Revision: 1, Epoch: 1},
		{Schema: 1, Revision: 2, Epoch: 1},
		{Schema: 1, Revision: 1, Epoch: 0},
		{Schema: 1, Revision: 1, Epoch: 1, Operations: []storage.Operation{{Key: "a", Value: json.RawMessage(`broken`)}}},
	} {
		if _, err := New(storage.Capabilities{}, []storage.Commit{c}, nil, nil); !errors.Is(err, storage.ErrCorrupt) {
			t.Fatalf("invalid history accepted: %v", err)
		}
	}
}
