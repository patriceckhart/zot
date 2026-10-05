package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type todos struct {
	Items []string `json:"items"`
}

func todoRegistry(t *testing.T, fork string, history bool) DocumentRegistry {
	t.Helper()
	reg := DocumentRegistry{}
	if err := reg.Register(DocumentDefinition{
		Kind: "app.todos", Version: 1, Scope: "conversation", Fork: fork, History: history,
		Initial: func() any { return todos{Items: []string{}} },
		Validate: func(v json.RawMessage) error {
			var td todos
			if err := json.Unmarshal(v, &td); err != nil {
				return err
			}
			for _, item := range td.Items {
				if strings.TrimSpace(item) == "" {
					return errors.New("empty item")
				}
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(DocumentDefinition{Kind: "app.settings", Version: 1, Scope: "runtime", Initial: func() any { return map[string]any{"theme": "dark"} }}); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestDocumentRegistryRules(t *testing.T) {
	reg := DocumentRegistry{}
	bad := []DocumentDefinition{
		{Kind: "", Version: 1, Scope: "conversation", Initial: func() any { return nil }},
		{Kind: "x", Version: 0, Scope: "conversation", Initial: func() any { return nil }},
		{Kind: "x", Version: 1, Scope: "global", Initial: func() any { return nil }},
		{Kind: "x", Version: 1, Scope: "conversation", Fork: "as_of", Initial: func() any { return nil }},
		{Kind: "x", Version: 1, Scope: "conversation", Fork: "sideways", Initial: func() any { return nil }},
		{Kind: "x", Version: 1, Scope: "runtime", Fork: "current", Initial: func() any { return nil }},
	}
	for i, def := range bad {
		if err := reg.Register(def); err == nil {
			t.Fatalf("definition %d accepted", i)
		}
	}
}

func TestDocumentWriteReadConflictDelete(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	reg := todoRegistry(t, "fresh", true)
	initial, err := r.ReadDocument(ctx, reg, "app.todos", c.ID)
	if err != nil || initial.Revision != 0 || string(initial.Value) != `{"items":[]}` {
		t.Fatalf("initial: %+v %v", initial, err)
	}
	if _, err := r.WriteDocument(ctx, reg, "app.todos", c.ID, 0, todos{Items: []string{" "}}); err == nil || !strings.Contains(err.Error(), "validation") {
		t.Fatalf("validation: %v", err)
	}
	first, err := r.WriteDocument(ctx, reg, "app.todos", c.ID, 0, todos{Items: []string{"write docs"}})
	if err != nil || first.Revision == 0 || first.Sequence != 1 {
		t.Fatalf("first write: %+v %v", first, err)
	}
	// Lost-update protection: a stale revision conflicts and returns the
	// current value.
	stale, err := r.WriteDocument(ctx, reg, "app.todos", c.ID, 0, todos{Items: []string{"clobber"}})
	if !errors.Is(err, ErrDocumentConflict) || string(stale.Value) != `{"items":["write docs"]}` {
		t.Fatalf("conflict: %+v %v", stale, err)
	}
	second, err := r.WriteDocument(ctx, reg, "app.todos", c.ID, first.Revision, todos{Items: []string{"write docs", "ship"}})
	if err != nil || second.Sequence != 2 {
		t.Fatalf("second write: %+v %v", second, err)
	}
	// Document writes advance the conversation revision so Configure
	// callers see a change.
	cur, _ := r.Conversation(ctx, c.ID)
	if cur.Revision != second.Revision {
		t.Fatalf("conversation revision: %d vs %d", cur.Revision, second.Revision)
	}
	deleted, err := r.DeleteDocument(ctx, reg, "app.todos", c.ID, second.Revision)
	if err != nil || !deleted.Deleted {
		t.Fatalf("delete: %+v %v", deleted, err)
	}
	after, _ := r.ReadDocument(ctx, reg, "app.todos", c.ID)
	if string(after.Value) != `{"items":[]}` || after.Revision != deleted.Revision {
		t.Fatalf("after delete: %+v", after)
	}
	// Runtime scope.
	if _, err := r.ReadDocument(ctx, reg, "app.settings", c.ID); err == nil {
		t.Fatal("runtime document read with conversation scope")
	}
	settings, err := r.ReadDocument(ctx, reg, "app.settings", "")
	if err != nil || !strings.Contains(string(settings.Value), "dark") {
		t.Fatalf("runtime read: %+v %v", settings, err)
	}
	if _, err := r.WriteDocument(ctx, reg, "app.settings", "", 0, map[string]any{"theme": "light"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadDocument(ctx, reg, "missing", c.ID); !errors.Is(err, ErrDocumentBlocked) {
		t.Fatalf("unknown kind: %v", err)
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestDocumentForkPolicies(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, newMemoryStore(), []scriptStep{{text: "one"}, {text: "two"}})
	defer h.r.Close()
	parent := h.root(t)
	// Entry 1 user, entry 2 assistant. Write v1 before, v2 after.
	for _, policy := range []struct {
		fork    string
		history bool
		want    string
	}{
		{"fresh", false, `{"items":[]}`},
		{"current", false, `{"items":["after"]}`},
		{"as_of", true, `{"items":["before"]}`},
	} {
		reg := todoRegistry(t, policy.fork, policy.history)
		kind := "app.todos"
		// Reset the document state between policies by using a new runtime
		// conversation per policy: simpler to fork a fresh root each time.
		root, err := h.r.OpenRoot(ctx, "workspace-"+policy.fork, AgentConfig{Provider: "synthetic", Model: "scripted"})
		if err != nil {
			t.Fatal(err)
		}
		_ = parent
		if _, err := h.r.WriteDocument(ctx, reg, kind, root.ID, 0, todos{Items: []string{"before"}}); err != nil {
			t.Fatal(err)
		}
		h.client.steps = []scriptStep{{text: "answer"}}
		h.r.Submit(ctx, root.ID, "actor", "", "hello")
		if run, _, err := h.svc.Step(ctx, root.ID); err != nil || run.Outcome != "completed" {
			t.Fatalf("%s: run %+v %v", policy.fork, run, err)
		}
		current, _ := h.r.ReadDocument(ctx, reg, kind, root.ID)
		if _, err := h.r.WriteDocument(ctx, reg, kind, root.ID, current.Revision, todos{Items: []string{"after"}}); err != nil {
			t.Fatal(err)
		}
		// Fork at entry 1 (the user message), before "after" was written at
		// entry sequence 2.
		child, err := h.r.Fork(ctx, root.ID, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		seen, err := h.r.ReadDocument(ctx, reg, kind, child.ID)
		if err != nil || string(seen.Value) != policy.want || seen.Revision != 0 || seen.ConversationID != child.ID {
			t.Fatalf("%s: fork sees %+v %v, want %s", policy.fork, seen, err, policy.want)
		}
		// A child write does not touch the parent.
		if _, err := h.r.WriteDocument(ctx, reg, kind, child.ID, 0, todos{Items: []string{"child"}}); err != nil {
			t.Fatalf("%s: child write %v", policy.fork, err)
		}
		parentDoc, _ := h.r.ReadDocument(ctx, reg, kind, root.ID)
		if string(parentDoc.Value) != `{"items":["after"]}` {
			t.Fatalf("%s: parent changed: %s", policy.fork, parentDoc.Value)
		}
		// Grandchild of the child inherits the child's value under current and
		// as_of, and nothing under fresh.
		h.client.steps = []scriptStep{{text: "child answer"}}
		h.r.Submit(ctx, child.ID, "actor", "", "child input")
		if run, _, err := h.svc.Step(ctx, child.ID); err != nil || run.Outcome != "completed" {
			t.Fatalf("%s: child run %+v %v", policy.fork, run, err)
		}
		grandchild, err := h.r.Fork(ctx, child.ID, 1, nil)
		if err != nil {
			t.Fatal(err)
		}
		gc, _ := h.r.ReadDocument(ctx, reg, kind, grandchild.ID)
		wantGC := `{"items":["child"]}`
		if policy.fork == "fresh" {
			wantGC = `{"items":[]}`
		}
		if string(gc.Value) != wantGC {
			t.Fatalf("%s: grandchild sees %s, want %s", policy.fork, gc.Value, wantGC)
		}
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

func TestDocumentMigration(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	v1 := DocumentRegistry{}
	v1.Register(DocumentDefinition{Kind: "cfg", Version: 1, Scope: "conversation", Initial: func() any { return map[string]any{"n": 1} }})
	if _, err := r.WriteDocument(ctx, v1, "cfg", c.ID, 0, map[string]any{"n": 5}); err != nil {
		t.Fatal(err)
	}
	v2 := DocumentRegistry{}
	v2.Register(DocumentDefinition{Kind: "cfg", Version: 2, Scope: "conversation", Initial: func() any { return map[string]any{"count": 1} }})
	if _, err := r.ReadDocument(ctx, v2, "cfg", c.ID); !errors.Is(err, ErrDocumentBlocked) {
		t.Fatalf("v2 without migration: %v", err)
	}
	def := v2["cfg"]
	def.Migrate = func(from int, value json.RawMessage) (json.RawMessage, error) {
		var old map[string]int
		if err := json.Unmarshal(value, &old); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]int{"count": old["n"]})
	}
	v2["cfg"] = def
	doc, err := r.ReadDocument(ctx, v2, "cfg", c.ID)
	if err != nil || doc.Version != 2 || string(doc.Value) != `{"count":5}` {
		t.Fatalf("migrated read: %+v %v", doc, err)
	}
	// Migration is in memory until written; the stored record stays v1.
	stored, _ := r.ReadDocument(ctx, v1, "cfg", c.ID)
	if stored.Version != 1 {
		t.Fatalf("read migrated the store: %+v", stored)
	}
	if _, err := r.WriteDocument(ctx, v2, "cfg", c.ID, doc.Revision, map[string]int{"count": 6}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadDocument(ctx, v1, "cfg", c.ID); !errors.Is(err, ErrDocumentBlocked) {
		t.Fatalf("old registry reading newer version: %v", err)
	}
}

func TestDocumentCommitsAtomicallyWithTaskCheckpoint(t *testing.T) {
	ctx := context.Background()
	r, c := newTaskRuntime(t, newMemoryStore())
	defer r.Close()
	docs := todoRegistry(t, "fresh", false)
	tasks := TaskRegistry{}
	tasks.Register(TaskDefinition{
		Kind: "planner", Version: 1,
		Initial: func(json.RawMessage) (Next, error) { return Next{Phase: "plan"}, nil },
		Phases: map[string]func(context.Context, TaskContext) (Next, error){
			"plan": func(ctx context.Context, tc TaskContext) (Next, error) {
				ops, err := DocumentWrite(tc.Snapshot, docs, "app.todos", tc.Task.ConversationID, todos{Items: []string{"from task"}})
				if err != nil {
					return Next{}, err
				}
				return Next{Outcome: "completed", Ops: ops}, nil
			},
		},
	})
	task, _ := r.CreateTask(ctx, tasks, c.ID, TaskSpec{Kind: "planner"})
	if err := NewTaskScheduler(r, tasks).Run(ctx); err != nil {
		t.Fatal(err)
	}
	final, _ := r.Task(ctx, task.ID)
	doc, _ := r.ReadDocument(ctx, docs, "app.todos", c.ID)
	if final.Outcome != "completed" || string(doc.Value) != `{"items":["from task"]}` || doc.Revision != final.Revision {
		t.Fatalf("not atomic: task=%+v doc=%+v", final, doc)
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}
