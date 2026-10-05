package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
)

func TestContinuousCheckState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	store, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := continuous.New(store)
	c, err := r.OpenRoot(ctx, "workspace", continuous.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, c.ID, "actor", "request", "private synthetic content"); err != nil {
		t.Fatal(err)
	}
	before, _ := r.Status(ctx)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runContinuous(ctx, []string{"check-state", "--store", path, "--durability", "process"}, &out); err != nil {
		t.Fatal(err)
	}
	var report continuous.Integrity
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Valid || report.Scope != "admission-state" || report.Revision != before.Revision+1 || report.Conversations != 1 || report.Submissions != 1 || report.Entries != 1 {
		t.Fatalf("report: %+v", report)
	}
	if strings.Contains(out.String(), c.ID) || strings.Contains(out.String(), "private synthetic content") {
		t.Fatal("private data in report")
	}
	// Runtime reference corruption is opaque valid JSON to journal framing checks.
	store, err = journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: store.Epoch(), Operations: []storage.Operation{{Key: "conversation/" + c.ID, Delete: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Verify(ctx, path); err != nil {
		t.Fatalf("journal framing should remain valid: %v", err)
	}
	out.Reset()
	err = runContinuous(ctx, []string{"check-state", "--store", path, "--durability", "process"}, &out)
	if !errors.Is(err, storage.ErrCorrupt) || out.Len() != 0 {
		t.Fatalf("corruption: %v, output %q", err, out.String())
	}
}

func TestContinuousCheckStateOptions(t *testing.T) {
	for _, args := range [][]string{
		{"check-state"},
		{"check-state", "extra", "--store", "store"},
		{"check-state", "--store", "store", "--limit", "1"},
		{"check-state", "--store", "store", "--output", "file"},
	} {
		if _, err := parseContinuousOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	path := filepath.Join(t.TempDir(), "missing")
	var out bytes.Buffer
	if err := runContinuous(context.Background(), []string{"check-state", "--store", path, "--durability", "process"}, &out); err == nil {
		t.Fatal("created missing store")
	}
}

func TestContinuousRecoverAbortUsageCommands(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	store, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := continuous.New(store)
	c, _ := r.OpenRoot(ctx, "workspace", continuous.AgentConfig{})
	r.Submit(ctx, c.ID, "actor", "", "hello")
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"recover", "--store", path},
		{"abort", "--store", path},
		{"usage", "--store", path},
		{"status", "--store", path, "--dry-run"},
	} {
		if _, err := parseContinuousOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	var out bytes.Buffer
	if err := runContinuous(ctx, []string{"recover", "--dry-run", "--store", path, "--durability", "process"}, &out); err != nil || !strings.Contains(out.String(), `"actions":[]`) {
		t.Fatalf("recover: %v %s", err, out.String())
	}
	out.Reset()
	if err := runContinuous(ctx, []string{"abort", c.ID, "--store", path, "--durability", "process"}, &out); !errors.Is(err, continuous.ErrNoRun) {
		t.Fatalf("abort without run: %v", err)
	}
	out.Reset()
	if err := runContinuous(ctx, []string{"usage", c.ID, "--store", path, "--durability", "process"}, &out); err != nil || !strings.Contains(out.String(), `"known":0`) {
		t.Fatalf("usage: %v %s", err, out.String())
	}
	if err := runContinuous(ctx, []string{"usage", "missing", "--store", path, "--durability", "process"}, &out); !errors.Is(err, continuous.ErrNotFound) {
		t.Fatalf("usage missing: %v", err)
	}
}

func TestContinuousInspectionCommands(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	store, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := continuous.New(store)
	c, err := r.OpenRoot(ctx, "workspace", continuous.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := r.Submit(ctx, c.ID, "actor", "", "find the needle")
	if err != nil {
		t.Fatal(err)
	}
	reg := continuous.TaskRegistry{}
	reg.Register(continuous.TaskDefinition{Kind: "noop", Version: 1,
		Initial: func(json.RawMessage) (continuous.Next, error) { return continuous.Next{Phase: "run"}, nil },
		Phases: map[string]func(context.Context, continuous.TaskContext) (continuous.Next, error){
			"run": func(context.Context, continuous.TaskContext) (continuous.Next, error) {
				return continuous.Next{Outcome: "completed"}, nil
			},
		}})
	task, err := r.CreateTask(ctx, reg, c.ID, continuous.TaskSpec{Kind: "noop", Background: true})
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	run := func(args ...string) string {
		var out bytes.Buffer
		if err := runContinuous(ctx, append(args, "--store", path, "--durability", "process"), &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if got := run("tasks", c.ID); !strings.Contains(got, task.ID) || !strings.Contains(got, `"background":true`) {
		t.Fatalf("tasks: %s", got)
	}
	if got := run("inspect", task.ID); !strings.Contains(got, `"kind":"noop"`) {
		t.Fatalf("inspect task: %s", got)
	}
	if got := run("inspect", sub.ID); !strings.Contains(got, `"state":"queued"`) {
		t.Fatalf("inspect submission: %s", got)
	}
	if got := run("inspect", c.ID); !strings.Contains(got, `"queue":[`) {
		t.Fatalf("inspect conversation: %s", got)
	}
	if got := run("approvals", c.ID); strings.TrimSpace(got) != "[]" {
		t.Fatalf("approvals: %s", got)
	}
	if got := run("search", c.ID, "--text", "NEEDLE"); !strings.Contains(got, `"sequence":1`) {
		t.Fatalf("search: %s", got)
	}
	if got := run("budget", c.ID, "--limit-tokens", "5"); !strings.Contains(got, `"limit_tokens":5`) || !strings.Contains(got, `"exceeded":false`) {
		t.Fatalf("budget set: %s", got)
	}
	if got := run("budget", c.ID, "--clear"); strings.TrimSpace(got) != `{"budgets":[],"exceeded":false}` {
		t.Fatalf("budget clear: %s", got)
	}
	// Aborting by task ID marks the task tree.
	if got := run("abort", task.ID, "--include-background"); !strings.Contains(got, `"abort_requested":true`) {
		t.Fatalf("task abort: %s", got)
	}
	var out bytes.Buffer
	if err := runContinuous(ctx, []string{"decide", "missing", "--store", path, "--durability", "process"}, &out); err == nil || !strings.Contains(err.Error(), "--allow or --deny") {
		t.Fatalf("decide without decision: %v", err)
	}
	if err := runContinuous(ctx, []string{"decide", "missing", "--allow", "--store", path, "--durability", "process"}, &out); !errors.Is(err, continuous.ErrApprovalNotFound) {
		t.Fatalf("decide missing: %v", err)
	}
}
