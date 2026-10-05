package continuous

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/continuous/storage/sqlite"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// The child process exits with os.Exit from inside the tool, after the tool
// intent was committed. No defers or Close run. The parent reopens the store
// and proves that the unsafe effect is reported, not repeated, while a tool
// declared replay-safe is rerun exactly once.
func TestExecutionProcessCrashInsideTool(t *testing.T) {
	if path := os.Getenv("ZOT_CONTINUOUS_EXEC_CRASH_STORE"); path != "" {
		ctx := context.Background()
		store, err := openCrashStore(ctx, os.Getenv("ZOT_CONTINUOUS_EXEC_CRASH_BACKEND"), path)
		if err != nil {
			t.Fatal(err)
		}
		marker := os.Getenv("ZOT_CONTINUOUS_EXEC_CRASH_MARKER")
		crash := &crashingTool{name: "unsafe", marker: marker}
		safe := &crashingTool{name: "safe", marker: marker, replay: core.ReplaySafe}
		h := newHarness(t, store, []scriptStep{
			{calls: []provider.ToolCallBlock{call("s1", "safe", `{}`), call("u1", "unsafe", `{}`)}},
		}, crash, safe)
		c := h.root(t)
		h.r.Submit(ctx, c.ID, "actor", "req", "hello")
		h.svc.Step(ctx, c.ID)
		t.Fatal("child survived the crash point")
	}
	for _, backend := range []string{"journal", "sqlite"} {
		t.Run(backend, func(t *testing.T) { crashInsideTool(t, backend) })
	}
}

// openCrashStore opens the backend named by the crash test environment.
func openCrashStore(ctx context.Context, backend, path string) (storage.Store, error) {
	if backend == "sqlite" {
		return sqlite.Open(ctx, path, sqlite.Options{Durability: storage.Process})
	}
	return journal.Open(ctx, path, journal.Options{Durability: storage.Process})
}

func crashInsideTool(t *testing.T, backend string) {
	dir := t.TempDir()
	path := filepath.Join(dir, "store")
	marker := filepath.Join(dir, "effects")
	cmd := exec.Command(os.Args[0], "-test.run=^TestExecutionProcessCrashInsideTool$")
	cmd.Env = append(os.Environ(), "ZOT_CONTINUOUS_EXEC_CRASH_STORE="+path, "ZOT_CONTINUOUS_EXEC_CRASH_MARKER="+marker, "ZOT_CONTINUOUS_EXEC_CRASH_BACKEND="+backend)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("child did not crash: %s", out)
	}
	effects := readEffects(t, marker)
	if effects != "safe unsafe" {
		t.Fatalf("child effects: %q", effects)
	}
	ctx := context.Background()
	store, err := openCrashStore(ctx, backend, path)
	if err != nil {
		t.Fatal(err)
	}
	unsafe := &crashingTool{name: "unsafe", marker: marker}
	safe := &crashingTool{name: "safe", marker: marker, replay: core.ReplaySafe}
	h := newHarness(t, store, []scriptStep{{text: "done"}}, unsafe, safe)
	defer h.r.Close()
	c, err := h.r.OpenRoot(ctx, "workspace", AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	run, ok, err := h.r.Run(ctx, c.ID)
	if err != nil || !ok || run.Phase != "tools" {
		t.Fatalf("interrupted run not persisted: %+v %v", run, err)
	}
	if run.Tools[0].State != "done" || run.Tools[1].State != "running" {
		// The safe tool finished and committed; the unsafe one died mid-call.
		t.Fatalf("intent states: %+v", run.Tools)
	}
	run, _, err = h.svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("recovery: %+v %v", run, err)
	}
	if effects := readEffects(t, marker); effects != "safe unsafe" {
		t.Fatalf("effects after recovery: %q (unsafe tool must not be repeated, safe tool already had its result)", effects)
	}
	results := h.client.requests[0].Messages[2].Content
	if len(results) != 2 {
		t.Fatalf("results: %+v", results)
	}
	safeResult, unsafeResult := results[0].(provider.ToolResultBlock), results[1].(provider.ToolResultBlock)
	if safeResult.IsError || !unsafeResult.IsError || !strings.Contains(core.MessageText(provider.Message{Content: unsafeResult.Content}), "interrupted") {
		t.Fatalf("results: safe=%+v unsafe=%+v", safeResult, unsafeResult)
	}
	if !strings.Contains(strings.Join(run.Notices, "\n"), "u1 interrupted") {
		t.Fatalf("notices: %v", run.Notices)
	}
	sub, err := h.r.Submit(ctx, c.ID, "actor", "req", "hello")
	if err != nil || sub.State != "answered" {
		t.Fatalf("submission: %+v %v", sub, err)
	}
	if report, err := h.r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}

type crashingTool struct {
	name   string
	marker string
	replay core.ToolReplayPolicy
}

func (t *crashingTool) Name() string                        { return t.name }
func (t *crashingTool) Description() string                 { return "records an effect" }
func (t *crashingTool) Schema() json.RawMessage             { return json.RawMessage(`{"type":"object"}`) }
func (t *crashingTool) ReplayPolicy() core.ToolReplayPolicy { return t.replay }
func (t *crashingTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	f, err := os.OpenFile(t.marker, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return core.ToolResult{}, err
	}
	f.WriteString(t.name + "\n")
	f.Close()
	if t.name == "unsafe" && os.Getenv("ZOT_CONTINUOUS_EXEC_CRASH_STORE") != "" {
		os.Exit(3)
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}}, nil
}

func readEffects(t *testing.T, marker string) string {
	t.Helper()
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(b)), " ")
}
