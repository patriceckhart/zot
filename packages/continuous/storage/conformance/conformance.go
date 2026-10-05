// Package conformance provides shared semantic checks for continuous backends.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Run requires a new empty store for each test. Persistent lifecycle and crash
// checks are backend-specific and must be run in addition to this suite.
func Run(t *testing.T, open func(*testing.T) storage.Store) {
	t.Helper()
	t.Run("AtomicSnapshotAndWatch", func(t *testing.T) {
		s := open(t)
		defer s.Close()
		ctx := context.Background()
		snap, err := s.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		raw := json.RawMessage(`{"value":1}`)
		c, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "a/1", Value: raw}, {Key: "a/2", Value: raw}}})
		if err != nil {
			t.Fatal(err)
		}
		raw[0] = 'x'
		c.Operations[0].Value[0] = 'x'
		if _, ok := snap.Get("a/1"); ok {
			t.Fatal("snapshot changed after commit")
		}
		// Commit between snapshot and subscription must not be missed.
		wait, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := s.Wait(wait, snap.Revision()); err != nil {
			t.Fatal(err)
		}
		commits, err := s.Scan(ctx, snap.Revision(), 1)
		if err != nil || len(commits) != 1 {
			t.Fatalf("scan: %v, %v", commits, err)
		}
		if string(commits[0].Operations[0].Value) != `{"value":1}` {
			t.Fatal("commit aliases caller memory")
		}
		current, err := s.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		page, err := current.Page("a/", "", 1)
		if err != nil || len(page) != 1 || page[0].Key != "a/1" {
			t.Fatalf("page: %v, %v", page, err)
		}
		page[0].Value[0] = 'x'
		next, err := current.Page("a/", page[0].Key, 1)
		if err != nil || len(next) != 1 || next[0].Key != "a/2" {
			t.Fatalf("next page: %v, %v", next, err)
		}
		b, _ := current.Get("a/1")
		if !json.Valid(b) {
			t.Fatal("page aliases snapshot")
		}
		_, err = s.Commit(ctx, storage.Mutation{ExpectedRevision: current.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "a/1", Delete: true}}})
		if err != nil {
			t.Fatal(err)
		}
		latest, _ := s.Snapshot(ctx)
		if _, ok := latest.Get("a/1"); ok {
			t.Fatal("delete not visible")
		}
		if _, ok := current.Get("a/1"); !ok {
			t.Fatal("delete changed old snapshot")
		}
	})
	t.Run("FailClosed", func(t *testing.T) {
		s := open(t)
		defer s.Close()
		ctx := context.Background()
		v, _ := s.Snapshot(ctx)
		m := storage.Mutation{ExpectedRevision: v.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "valid", Value: json.RawMessage(`1`)}, {Key: "invalid", Value: json.RawMessage(`broken`)}}}
		if _, err := s.Commit(ctx, m); err == nil {
			t.Fatal("invalid operation accepted")
		}
		after, _ := s.Snapshot(ctx)
		if after.Revision() != v.Revision() {
			t.Fatal("invalid commit published")
		}
		if _, ok := after.Get("valid"); ok {
			t.Fatal("partial transaction")
		}
		m.Operations = nil
		m.Epoch--
		if _, err := s.Commit(ctx, m); !errors.Is(err, storage.ErrFenced) {
			t.Fatalf("fencing: %v", err)
		}
		m.Epoch = s.Epoch()
		m.ExpectedRevision--
		if _, err := s.Commit(ctx, m); !errors.Is(err, storage.ErrConflict) {
			t.Fatalf("conflict: %v", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		m.ExpectedRevision = v.Revision()
		if _, err := s.Commit(cancelled, m); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
		if err := s.Wait(cancelled, v.Revision()); !errors.Is(err, context.Canceled) {
			t.Fatalf("wait cancel: %v", err)
		}
		if _, err := s.Scan(ctx, v.Revision()+1, 1); !errors.Is(err, storage.ErrCursor) {
			t.Fatalf("cursor: %v", err)
		}
		if _, err := v.Page("", "", 0); err == nil {
			t.Fatal("unbounded page accepted")
		}
	})
	t.Run("CloseUnblocksWait", func(t *testing.T) {
		s := open(t)
		defer s.Close()
		v, _ := s.Snapshot(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() { result <- s.Wait(ctx, v.Revision()) }()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-result; !errors.Is(err, storage.ErrClosed) {
			t.Fatalf("close: %v", err)
		}
		if _, err := s.Snapshot(context.Background()); !errors.Is(err, storage.ErrClosed) {
			t.Fatalf("snapshot after close: %v", err)
		}
	})
}
