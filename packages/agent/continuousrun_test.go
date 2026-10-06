package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// fakeChatServer serves OpenAI-compatible streaming completions from a script
// and records every request body. Credentials are synthetic.
type fakeChatServer struct {
	mu       sync.Mutex
	script   []string
	requests []map[string]any
}

func (f *fakeChatServer) handler(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	if len(f.script) == 0 {
		f.mu.Unlock()
		http.Error(w, `{"error":{"message":"script exhausted"}}`, http.StatusInternalServerError)
		return
	}
	chunk := f.script[0]
	f.script = f.script[1:]
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, chunk)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func textChunk(text string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n", text)
}

func toolChunk(id, name, args string) string {
	argsJSON, _ := json.Marshal(args)
	return fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]},\"finish_reason\":null}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n", id, name, argsJSON)
}

func TestContinuousRunExecutesWithHostConfiguration(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "synthetic-key")
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "note.txt"), []byte("synthetic file contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &fakeChatServer{script: []string{
		toolChunk("call_1", "read", `{"path":"note.txt"}`),
		textChunk("The note says: synthetic file contents"),
	}}
	srv := httptest.NewServer(http.HandlerFunc(server.handler))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "store")
	var out bytes.Buffer
	args := []string{"answer from the note", "--store", store, "--durability", "process", "--request-id", "req-1", "--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--cwd", cwd, "--tools", "read", "--no-ext", "--no-skills"}
	if err := runContinuousRun(context.Background(), args, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "The note says") {
		t.Fatalf("answer not printed: %q", out.String())
	}
	if len(server.requests) != 2 {
		t.Fatalf("requests: %d", len(server.requests))
	}
	second, _ := json.Marshal(server.requests[1]["messages"])
	if !strings.Contains(string(second), "synthetic file contents") || !strings.Contains(string(second), `"tool_call_id":"call_1"`) {
		t.Fatalf("tool result not sent to provider: %s", second)
	}
	if !strings.Contains(fmt.Sprint(server.requests[1]["tools"]), "read") {
		t.Fatalf("host tool registry not applied: %v", server.requests[1]["tools"])
	}
	// Identical retry: nothing new executes, the answered submission is returned.
	out.Reset()
	server.script = nil
	if err := runContinuousRun(context.Background(), append(args, "--json"), &out); err != nil {
		t.Fatalf("retry: %v\n%s", err, out.String())
	}
	if len(server.requests) != 2 || !strings.Contains(out.String(), `"state":"answered"`) || !strings.Contains(out.String(), `"type":"run_end"`) {
		t.Fatalf("retry executed work: requests=%d out=%s", len(server.requests), out.String())
	}
	// The store is inspectable with the offline commands.
	out.Reset()
	if err := runContinuous(context.Background(), []string{"check-state", "--store", store, "--durability", "process"}, &out); err != nil || !strings.Contains(out.String(), `"valid":true`) {
		t.Fatalf("check-state: %v %s", err, out.String())
	}
}

func TestContinuousRunResumesInterruptedToolRound(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "synthetic-key")
	cwd := t.TempDir()
	server := &fakeChatServer{script: []string{textChunk("recovered answer")}}
	srv := httptest.NewServer(http.HandlerFunc(server.handler))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "store")
	// Simulate a host that died inside a bash call: commit admission, the
	// assistant tool call, and a running intent for a tool without a replay
	// policy, using the runtime directly.
	ctx := context.Background()
	js, err := journal.Open(ctx, store, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	rt, _ := continuous.New(js)
	root, err := rt.OpenRoot(ctx, cwd, continuous.AgentConfig{Provider: "openai", Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Submit(ctx, root.ID, "cli:test", "", "delete the temp files"); err != nil {
		t.Fatal(err)
	}
	dying := &dyingTool{}
	svc, _ := continuous.NewService(rt, continuous.EngineFunc(func(ctx context.Context, c continuous.Conversation) (*core.Agent, error) {
		return core.NewAgent(&scriptedToolClient{}, "gpt-4o-mini", "", core.NewRegistry(dying)), nil
	}), continuous.ExecutionOptions{})
	stepCtx, cancel := context.WithCancel(ctx)
	dying.cancel = cancel
	_, _, err = svc.Step(stepCtx, root.ID)
	if err == nil {
		t.Fatal("expected interruption")
	}
	run, ok, _ := rt.Run(ctx, root.ID)
	if !ok || run.Phase != "tools" || run.Tools[0].State != "running" {
		t.Fatalf("interrupted run: %+v", run)
	}
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	args := []string{"--resume", "--store", store, "--durability", "process", "--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--cwd", cwd, "--tools", "bash", "--no-ext", "--no-skills", "--workspace", cwd}
	if err := runContinuousRun(ctx, args, &out); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "recovered answer") {
		t.Fatalf("resume output: %q", out.String())
	}
	if len(server.requests) != 1 {
		t.Fatalf("requests: %d", len(server.requests))
	}
	msgs, _ := json.Marshal(server.requests[0]["messages"])
	if !strings.Contains(string(msgs), "interrupted") || !strings.Contains(string(msgs), `"tool_call_id":"bash_1"`) {
		t.Fatalf("interrupted bash not reported to the model: %s", msgs)
	}
	if dying.runs != 1 {
		t.Fatalf("bash repeated after crash: %d", dying.runs)
	}
}

// dyingTool is named bash so the resumed host resolves the same tool name,
// with the default ReplayNever policy. Its first execution cancels the step.
type dyingTool struct {
	cancel context.CancelFunc
	runs   int
}

func (d *dyingTool) Name() string            { return "bash" }
func (d *dyingTool) Description() string     { return "synthetic shell" }
func (d *dyingTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (d *dyingTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	d.runs++
	d.cancel()
	<-ctx.Done()
	return core.ToolResult{}, ctx.Err()
}

type scriptedToolClient struct{}

func (scriptedToolClient) Name() string { return "openai" }
func (scriptedToolClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 2)
	msg := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: "bash_1", Name: "bash", Arguments: json.RawMessage(`{"command":"rm -rf tmp"}`)}}}
	ch <- provider.EventDone{Stop: provider.StopToolUse, Message: msg}
	close(ch)
	return ch, nil
}

// continuous run executes on durable tasks with the host's ordinary
// configuration, and migrate-runs reports nothing left to convert.
func TestContinuousRunTasksExecutor(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "synthetic-key")
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "note.txt"), []byte("synthetic file contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &fakeChatServer{script: []string{
		toolChunk("call_1", "read", `{"path":"note.txt"}`),
		textChunk("The note says: synthetic file contents"),
	}}
	srv := httptest.NewServer(http.HandlerFunc(server.handler))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "store")
	var out bytes.Buffer
	args := []string{"answer from the note", "--store", store, "--durability", "process", "--request-id", "req-1", "--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--cwd", cwd, "--tools", "read", "--no-ext", "--no-skills"}
	if err := runContinuousRun(context.Background(), args, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "The note says") || len(server.requests) != 2 {
		t.Fatalf("answer: %q requests=%d", out.String(), len(server.requests))
	}
	second, _ := json.Marshal(server.requests[1]["messages"])
	if !strings.Contains(string(second), `"tool_call_id":"call_1"`) {
		t.Fatalf("tool result not sent: %s", second)
	}
	out.Reset()
	if err := runContinuous(context.Background(), []string{"check-state", "--store", store, "--durability", "process"}, &out); err != nil || !strings.Contains(out.String(), `"valid":true`) {
		t.Fatalf("check-state: %v %s", err, out.String())
	}
	out.Reset()
	if err := runContinuous(context.Background(), []string{"migrate-runs", "--store", store, "--durability", "process"}, &out); err != nil || !strings.Contains(out.String(), `"migrated":0`) {
		t.Fatalf("migrate-runs: %v %s", err, out.String())
	}
}
