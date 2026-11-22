package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// This subprocess is a real protocol peer. All data is synthetic and lives in
// t.TempDir; the parent reads the captured wire frames only after shutdown.
func TestLifecycleProcessHelper(t *testing.T) {
	if os.Getenv("ZOT_TEST_LIFECYCLE") != "1" {
		return
	}
	file, err := os.Create(os.Getenv("ZOT_TEST_LIFECYCLE_OUTPUT"))
	if err != nil {
		os.Exit(2)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(extproto.HelloFromExt{Type: "hello", Name: "lifecycle", Version: "1"})
	enc.Encode(extproto.SubscribeFromExt{Type: "subscribe", Events: []string{"session_start", "session_end", "user_prompt_submit", "turn_start", "turn_end", "tool_call", "tool_result", "permission_decision", "pre_compact", "post_compact"}, Intercept: []string{"tool_call"}})
	enc.Encode(extproto.ReadyFromExt{Type: "ready"})
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var e extproto.EventInterceptFromHost
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			os.Exit(3)
		}
		switch e.Type {
		case "event":
			if _, err := file.Write(append(append([]byte(nil), scanner.Bytes()...), '\n')); err != nil {
				os.Exit(4)
			}
		case "event_intercept":
			enc.Encode(extproto.EventInterceptResponseFromExt{Type: "event_intercept_response", ID: e.ID, Block: os.Getenv("ZOT_TEST_LIFECYCLE_BLOCK") == "1", Reason: "test policy", ModifiedArgs: json.RawMessage(`{"effective":true}`)})
		case "shutdown":
			file.Close()
			enc.Encode(extproto.ShutdownAckFromExt{Type: "shutdown_ack"})
			os.Exit(0)
		}
	}
	file.Close()
	os.Exit(0)
}

type lifecycleClient struct{ step int }

func (*lifecycleClient) Name() string { return "test" }
func (c *lifecycleClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	c.step++
	stop := provider.StopEnd
	content := []provider.Content{provider.TextBlock{Text: "done"}}
	if c.step == 1 {
		stop = provider.StopToolUse
		content = []provider.Content{provider.ToolCallBlock{ID: "call-1", Name: "probe", Arguments: json.RawMessage(`{"original":true}`)}}
	}
	ch := make(chan provider.Event, 1)
	ch <- provider.EventDone{Stop: stop, Message: provider.Message{Role: provider.RoleAssistant, Content: content}}
	close(ch)
	return ch, nil
}

type lifecycleTool struct{ args json.RawMessage }

func (*lifecycleTool) Name() string            { return "probe" }
func (*lifecycleTool) Description() string     { return "test" }
func (*lifecycleTool) Schema() json.RawMessage { return json.RawMessage(`{}`) }
func (t *lifecycleTool) Execute(_ context.Context, args json.RawMessage, _ func(string)) (core.ToolResult, error) {
	t.args = append(json.RawMessage(nil), args...)
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "ok"}}}, nil
}

func TestExtensionLifecycleEndToEnd(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		name := "rewritten"
		if blocked {
			name = "blocked"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "events.jsonl")
			t.Setenv("ZOT_TEST_LIFECYCLE", "1")
			t.Setenv("ZOT_TEST_LIFECYCLE_OUTPUT", output)
			if blocked {
				t.Setenv("ZOT_TEST_LIFECYCLE_BLOCK", "1")
			} else {
				t.Setenv("ZOT_TEST_LIFECYCLE_BLOCK", "0")
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			manifest, err := json.Marshal(extensions.Manifest{Name: "lifecycle", Exec: exe, Args: []string{"-test.run=^TestLifecycleProcessHelper$"}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "extension.json"), manifest, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			mgr := extensions.New(t.TempDir(), dir, "test", "test", "test", nil)
			if errs := mgr.LoadExplicit(ctx, []string{dir}); len(errs) > 0 {
				t.Fatal(errs)
			}
			t.Cleanup(func() { mgr.Stop(time.Second) })
			mgr.WaitForReady(time.Second)
			tool := &lifecycleTool{}
			ag := core.NewAgent(&lifecycleClient{}, "test", "", core.NewRegistry(tool))
			bindAgentSession(ag, &core.Session{ID: "session-one", Meta: core.SessionMeta{Alias: "tidy-birch"}})
			wireNonInteractiveAgentExtHooks(ctx, ag, mgr)
			if err := ag.Prompt(ctx, "review", nil, func(e core.AgentEvent) {
				if _, ok := e.(core.EvPromptSubmit); ok {
					t.Error("extension-only submission leaked into the RPC/SDK stream")
				}
			}); err != nil {
				t.Fatal(err)
			}
			// Exercise the same explicit boundary helper used by the session picker.
			bindAgentSession(ag, &core.Session{ID: "session-two", Meta: core.SessionMeta{Alias: "chatty-lake"}})
			startExtensionSession(mgr, ag, dir, "session_switch")
			mgr.Stop(time.Second)
			data, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var events []extproto.EventFromHost
			scanner := bufio.NewScanner(strings.NewReader(string(data)))
			for scanner.Scan() {
				var e extproto.EventFromHost
				if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
					t.Fatal(err)
				}
				if e.Sequence == 0 || (len(events) > 0 && e.Sequence <= events[len(events)-1].Sequence) {
					t.Fatalf("wire sequence: %+v", e)
				}
				events = append(events, e)
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			indexes := map[string]int{}
			for n, e := range events {
				if _, exists := indexes[e.Event]; !exists {
					indexes[e.Event] = n
				}
				if e.SessionID == "" || e.CWD == "" {
					t.Fatalf("missing correlation: %+v", e)
				}
				wantAlias := "tidy-birch"
				if e.SessionID == "session-two" {
					wantAlias = "chatty-lake"
				}
				if e.SessionAlias != wantAlias {
					t.Fatalf("session alias = %q, want %q: %+v", e.SessionAlias, wantAlias, e)
				}
			}
			names := []string{"session_start", "user_prompt_submit", "turn_start", "tool_call", "turn_end", "permission_decision", "tool_result", "session_end"}
			previous := -1
			for _, name := range names {
				n, ok := indexes[name]
				if !ok || n <= previous {
					t.Fatalf("missing/out-of-order %s: %+v", name, events)
				}
				previous = n
			}
			result := events[indexes["tool_result"]]
			if result.ToolID != "call-1" || result.ToolName != "probe" || result.Result == nil || result.Executed == nil {
				t.Fatalf("result: %+v", result)
			}
			if blocked {
				if result.Status != "blocked" || *result.Executed || tool.args != nil || !result.Result.IsError {
					t.Fatalf("blocked outcome: %+v", result)
				}
			} else {
				if result.Status != "completed" || !*result.Executed || string(result.ToolArgs) != string(tool.args) || string(tool.args) != `{"effective":true}` {
					t.Fatalf("effective result: %+v", result)
				}
			}
			tail := events[len(events)-3:]
			if tail[0].Reason != "session_switch" || tail[0].SessionID != "session-one" || tail[1].Event != "session_start" || tail[1].SessionID != "session-two" || tail[2].Reason != "shutdown" {
				t.Fatalf("session boundaries: %+v", tail)
			}
		})
	}
}

func TestEventResultPayloadBudget(t *testing.T) {
	result := core.ToolResult{Content: []provider.Content{
		provider.TextBlock{Text: strings.Repeat("ü", eventResultBudget)},
		provider.ImageBlock{MimeType: "image/png", Data: []byte("synthetic")},
	}}
	out := extensionEventResult(result)
	if !out.Truncated || len(out.Content) != 1 || len(out.Content[0].Text) > eventResultBudget || !utf8.ValidString(out.Content[0].Text) {
		t.Fatalf("invalid bounded result")
	}
	if len(result.Content[0].(provider.TextBlock).Text) != 2*eventResultBudget {
		t.Fatal("transcript modified")
	}
	small := extensionEventResult(core.ToolResult{Content: []provider.Content{provider.ImageBlock{MimeType: "image/png", Data: []byte("test")}}})
	if small.Truncated || len(small.Content) != 1 || small.Content[0].Data != "dGVzdA==" {
		t.Fatalf("image: %+v", small)
	}
}
