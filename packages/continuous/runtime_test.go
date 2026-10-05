package continuous

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/continuous/storage/memory"
)

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, err := New(memory.Open())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func TestAdmission(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)
	original := AgentConfig{Provider: "synthetic", Model: "first"}
	c, err := r.OpenRoot(ctx, "workspace", original)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.OpenRoot(ctx, "workspace", AgentConfig{Model: "other"})
	if err != nil || again.ID != c.ID || !sameJSON(again.Config, original) {
		t.Fatalf("root changed: %+v, %v", again, err)
	}
	before, _ := r.Snapshot(ctx)
	s, err := r.Submit(ctx, c.ID, "actor", "request", "input")
	if err != nil {
		t.Fatal(err)
	}
	repeat, err := r.Submit(ctx, c.ID, "actor", "request", "input")
	if err != nil || repeat != s {
		t.Fatalf("duplicate: %+v, %v", repeat, err)
	}
	if _, err := r.Submit(ctx, c.ID, "actor", "request", "different"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("payload conflict: %v", err)
	}
	commits, err := r.Scan(ctx, before.Revision(), 10)
	if err != nil || len(commits) != 1 || len(commits[0].Operations) != 5 {
		t.Fatalf("admission not atomic: %v, %v", commits, err)
	}
	after, _ := r.Snapshot(ctx)
	for _, prefix := range []string{"queue/", "entry/", "submission/", "dedup/"} {
		records, err := after.Page(prefix, "", 10)
		if err != nil || len(records) != 1 {
			t.Fatalf("%s: %v, %v", prefix, records, err)
		}
	}
	other, err := r.Submit(ctx, c.ID, "other-actor", "request", "different")
	if err != nil || other.ID == s.ID || other.Sequence != 2 {
		t.Fatalf("actor namespace: %+v, %v", other, err)
	}
}
func TestConcurrentAdmission(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	c, err := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	const n = 32
	results := make(chan Submission, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := r.Submit(ctx, c.ID, "actor", "same-request", "same-input")
			results <- s
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var id string
	for s := range results {
		if id != "" && s.ID != id {
			t.Fatal("duplicate work admitted")
		}
		id = s.ID
	}
	current, _ := r.Conversation(ctx, c.ID)
	if current.QueueSequence != 1 {
		t.Fatalf("queue sequence: %d", current.QueueSequence)
	}
}
func TestConcurrentConfiguration(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	c, _ := r.OpenRoot(ctx, "workspace", AgentConfig{})
	// An unrelated conversation changes the global revision, not this config.
	if _, err := r.OpenRoot(ctx, "other", AgentConfig{}); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for _, model := range []string{"one", "two"} {
		go func(model string) {
			_, err := r.Configure(ctx, c.ID, c.Revision, AgentConfig{Model: model})
			results <- err
		}(model)
	}
	successes, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		err := <-results
		if err == nil {
			successes++
		} else if errors.Is(err, storage.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}
func TestPersistentAdmission(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	// Process mode exercises portable admission semantics. Strict barriers are
	// tested separately by the journal package on supported platforms.
	open := func() *Runtime {
		s, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
		if err != nil {
			t.Fatal(err)
		}
		r, err := New(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := open()
	c, err := r.OpenRoot(ctx, "workspace", AgentConfig{Model: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.Submit(ctx, c.ID, "actor", "request", "input")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = open()
	defer r.Close()
	again, err := r.OpenRoot(ctx, "workspace", AgentConfig{Model: "wrong"})
	if err != nil || again.ID != c.ID || !sameJSON(again.Config, c.Config) {
		t.Fatalf("reopened root: %+v, %v", again, err)
	}
	retry, err := r.Submit(ctx, c.ID, "actor", "request", "input")
	if err != nil || retry != first {
		t.Fatalf("restart dedup: %+v, %v", retry, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.Wait(cancelled, first.Revision); !errors.Is(err, context.Canceled) {
		t.Fatalf("observation: %v", err)
	}
	current, _ := r.Conversation(ctx, c.ID)
	if current.QueueSequence != 1 {
		t.Fatal("observation cancellation changed work")
	}
}
func TestInvalidAdmission(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	if _, err := New(nil); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := r.OpenRoot(ctx, " ", AgentConfig{}); err == nil {
		t.Fatal("empty workspace accepted")
	}
	if _, err := r.Submit(ctx, "missing", "actor", "id", "text"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	c, _ := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if _, err := r.Submit(ctx, c.ID, "", "id", "text"); err == nil {
		t.Fatal("empty actor accepted")
	}
	if _, err := r.Submit(ctx, c.ID, "actor", "id", " "); err == nil {
		t.Fatal("empty content accepted")
	}
}
