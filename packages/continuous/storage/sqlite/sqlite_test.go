package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/conformance"
)

func testOptions() Options { return Options{Durability: storage.Process} }

func openTest(t *testing.T, path string) storage.Store {
	t.Helper()
	s, err := Open(context.Background(), path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func commitKV(t *testing.T, s storage.Store, ops ...storage.Operation) storage.Commit {
	t.Helper()
	v, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Commit(context.Background(), storage.Mutation{ExpectedRevision: v.Revision(), Epoch: s.Epoch(), Actor: "test", Operations: ops})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) storage.Store { return openTest(t, filepath.Join(t.TempDir(), "store")) })
}

func TestCapabilities(t *testing.T) {
	s := openTest(t, filepath.Join(t.TempDir(), "store"))
	defer s.Close()
	caps := s.Capabilities()
	if caps.Durability != storage.Process || !caps.Indexed || !caps.BoundedHistoryMemory {
		t.Fatalf("capabilities: %+v", caps)
	}
	strict, err := Open(context.Background(), filepath.Join(t.TempDir(), "strict"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer strict.Close()
	if strict.Capabilities().Durability != storage.Strict {
		t.Fatalf("default durability: %+v", strict.Capabilities())
	}
	if _, err := Open(context.Background(), filepath.Join(t.TempDir(), "bad"), Options{Durability: "sometimes"}); err == nil {
		t.Fatal("unknown durability accepted")
	}
}

// Records, deletes, and history survive reopen; the epoch advances and an
// old epoch is fenced; a second writer is locked out.
func TestReopenFenceAndLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	s := openTest(t, path)
	epoch := s.Epoch()
	commitKV(t, s, storage.Operation{Key: "a", Value: json.RawMessage(`1`)}, storage.Operation{Key: "b", Value: json.RawMessage(`2`)})
	commitKV(t, s, storage.Operation{Key: "a", Value: json.RawMessage(`11`)}, storage.Operation{Key: "b", Delete: true})
	if _, err := Open(ctx, path, testOptions()); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("second writer: %v", err)
	}
	before, _ := s.Snapshot(ctx)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := before.Page("", "", 10); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("page after close: %v", err)
	}
	next := openTest(t, path)
	defer next.Close()
	if next.Epoch() != epoch+1 {
		t.Fatalf("epoch: %d -> %d", epoch, next.Epoch())
	}
	v, _ := next.Snapshot(ctx)
	if v.Revision() != before.Revision()+1 {
		t.Fatalf("revision after reopen: %d", v.Revision())
	}
	if b, ok := v.Get("a"); !ok || string(b) != `11` {
		t.Fatalf("a: %s %v", b, ok)
	}
	if _, ok := v.Get("b"); ok {
		t.Fatal("deleted key visible")
	}
	page, err := v.Page("", "", 10)
	if err != nil || len(page) != 1 || page[0].Key != "a" {
		t.Fatalf("page: %v %v", page, err)
	}
	if _, err := next.Commit(ctx, storage.Mutation{ExpectedRevision: v.Revision(), Epoch: epoch}); !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("old epoch: %v", err)
	}
	commits, err := next.Scan(ctx, 0, 10)
	if err != nil || len(commits) != 4 {
		t.Fatalf("history: %d %v", len(commits), err)
	}
	if commits[0].Actor != "writer.acquire" || commits[3].Actor != "writer.acquire" || commits[1].Operations[1].Key != "b" || !commits[2].Operations[1].Delete || string(commits[2].Operations[0].Value) != `11` {
		t.Fatalf("history content: %+v", commits)
	}
	if commits[1].Time.IsZero() {
		t.Fatal("commit time lost")
	}
}

// Snapshots are stable points in time: a snapshot taken before a commit
// keeps returning the old versions, and prefix pagination with deletes
// interleaved returns exactly the live keys in order.
func TestSnapshotIsolationAndPrefixPaging(t *testing.T) {
	ctx := context.Background()
	s := openTest(t, filepath.Join(t.TempDir(), "store"))
	defer s.Close()
	var ops []storage.Operation
	for i := 0; i < 25; i++ {
		ops = append(ops, storage.Operation{Key: fmt.Sprintf("entry/c/%03d", i), Value: json.RawMessage(fmt.Sprint(i))})
	}
	ops = append(ops, storage.Operation{Key: "entry/d/000", Value: json.RawMessage(`"other"`)}, storage.Operation{Key: "entry", Value: json.RawMessage(`"bare"`)}, storage.Operation{Key: "entry0", Value: json.RawMessage(`"sibling"`)})
	commitKV(t, s, ops...)
	old, _ := s.Snapshot(ctx)
	// Delete every even entry and overwrite every fifth.
	var changes []storage.Operation
	for i := 0; i < 25; i++ {
		key := fmt.Sprintf("entry/c/%03d", i)
		switch {
		case i%2 == 0:
			changes = append(changes, storage.Operation{Key: key, Delete: true})
		case i%5 == 0:
			changes = append(changes, storage.Operation{Key: key, Value: json.RawMessage(`"five"`)})
		}
	}
	commitKV(t, s, changes...)
	current, _ := s.Snapshot(ctx)
	if page, _ := old.Page("entry/c/", "", 1000); len(page) != 25 || string(page[10].Value) != "10" {
		t.Fatalf("old snapshot changed: %d", len(page))
	}
	var keys []string
	after := ""
	for {
		page, err := current.Page("entry/c/", after, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			keys = append(keys, r.Key)
			if r.Key == "entry/c/015" && string(r.Value) != `"five"` {
				t.Fatalf("overwrite lost: %s", r.Value)
			}
		}
		after = page[len(page)-1].Key
	}
	if len(keys) != 12 || keys[0] != "entry/c/001" || keys[11] != "entry/c/023" {
		t.Fatalf("live keys: %v", keys)
	}
	if _, ok := current.Get("entry/c/000"); ok {
		t.Fatal("deleted key visible")
	}
	if b, ok := current.Get("entry/c/005"); !ok || string(b) != `"five"` {
		t.Fatalf("overwritten key: %s %v", b, ok)
	}
	// Prefix boundaries: "entry/" excludes "entry" and "entry0".
	all, _ := current.Page("entry/", "", 1000)
	if len(all) != 13 || all[12].Key != "entry/d/000" {
		t.Fatalf("prefix page: %d %s", len(all), all[len(all)-1].Key)
	}
	bare, _ := current.Page("entry", "", 1000)
	if len(bare) != 15 || bare[0].Key != "entry" || bare[14].Key != "entry0" {
		t.Fatalf("bare prefix: %d", len(bare))
	}
	everything, _ := current.Page("", "", 1000)
	if len(everything) != 15 {
		t.Fatalf("empty prefix: %d", len(everything))
	}
	// Continuing from a cursor outside the prefix starts at the prefix.
	restart, _ := current.Page("entry/d/", "aaa", 10)
	if len(restart) != 1 {
		t.Fatalf("cursor below prefix: %v", restart)
	}
}

// Tampering with the database outside the API fails closed on open.
func TestTamperedCountersFailClosed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	s := openTest(t, path)
	commitKV(t, s, storage.Operation{Key: "a", Value: json.RawMessage(`1`)})
	s.Close()
	conn, err := openConn(filepath.Join(path, DatabaseFile), storage.Process, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec("UPDATE meta SET value = value + 5 WHERE name = 'revision'"); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := Open(ctx, path, testOptions()); !errors.Is(err, storage.ErrCorrupt) {
		t.Fatalf("tampered counters accepted: %v", err)
	}
	// A store directory that is a file is refused.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, file, testOptions()); err == nil {
		t.Fatal("file accepted as store")
	}
}

// Large histories do not require loading records into memory: opening a
// store with many commits reads only counters, and a page reads only its
// rows. This is a functional check of the access pattern, not a benchmark.
func TestOpenDoesNotReplayHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	s := openTest(t, path)
	for i := 0; i < 300; i++ {
		commitKV(t, s, storage.Operation{Key: fmt.Sprintf("k/%04d", i%50), Value: json.RawMessage(fmt.Sprint(i))})
	}
	s.Close()
	reopened := openTest(t, path)
	defer reopened.Close()
	v, _ := reopened.Snapshot(ctx)
	if v.Revision() != 302 {
		t.Fatalf("revision: %d", v.Revision())
	}
	page, err := v.Page("k/", "", 5)
	if err != nil || len(page) != 5 || string(page[0].Value) != "250" {
		t.Fatalf("page after reopen: %v %v", page, err)
	}
	commits, err := reopened.Scan(ctx, 150, 3)
	if err != nil || len(commits) != 3 || commits[0].Revision != 151 {
		t.Fatalf("scan: %v %v", commits, err)
	}
}
