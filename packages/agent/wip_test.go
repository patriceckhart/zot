package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestPickedLegacySessionWIP(t *testing.T) {
	for _, defaultOn := range []bool{false, true} {
		t.Run(fmt.Sprint(defaultOn), func(t *testing.T) {
			s, err := core.NewSession(t.TempDir(), t.TempDir(), "test", "model", "test")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			a := core.NewAgent(nil, "model", "", nil)
			a.WIPSettings.DefaultOn = defaultOn
			a.WIPBaseline = func() string { return "old" }
			a.SetWIP(true)
			a.WIPBaseline = func() string { return "new" }
			var msgs []provider.Message
			for range 75 {
				msgs = append(msgs,
					provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "question"}}},
					provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "answer"}}})
			}
			window := trimMessagesForResume(msgs, 100)
			bindPickedSession(a, s, window)
			state := a.WIPState()
			if state.Active != defaultOn || s.Meta.WIP == nil || *s.Meta.WIP != state {
				t.Fatalf("unexpected picked state: %+v", state)
			}
			a.SetWIP(true)
			if got := a.WIPState(); got.BaselineTurn != 75 || got.BaselineCommit != "new" {
				t.Fatalf("incorrect phase boundary: %+v", got)
			}
		})
	}
}

func TestStartupLegacySessionWIP(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	for _, key := range []string{"ZOT_SESSION_ID", "ZOT_SESSION_FILE", "ZOT_PROVIDER", "ZOT_MODEL", "ZOT_REASONING_LEVEL"} {
		t.Setenv(key, "")
	}
	for _, flag := range []bool{true, false} {
		cwd := t.TempDir()
		s, err := core.NewSession(t.TempDir(), cwd, "test", "model", "test")
		if err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "before"}}}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		a := core.NewAgent(nil, "model", "", nil)
		r := Resolved{CWD: cwd, WIPOn: flag, WIP: core.WIPSettings{DefaultOn: !flag}}
		configureWIP(a, r)
		resumed, err := openOrCreateSession(Args{CWD: cwd, Session: s.Path, WIP: flag}, r, a, "test")
		if err != nil {
			t.Fatal(err)
		}
		resumed.Close()
		state := a.WIPState()
		if !state.Active || state.BaselineTurn != 1 || resumed.Meta.WIP == nil || *resumed.Meta.WIP != state {
			t.Fatalf("flag=%v: incorrect resumed phase %+v", flag, state)
		}
	}
}

func TestWIPResumeToolOnlyTail(t *testing.T) {
	var msgs []provider.Message
	for range 150 {
		msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "question"}}})
	}
	for range 100 {
		msgs = append(msgs, provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call"}}})
	}
	window := trimMessagesForResume(msgs, 100)
	if len(window) > 100 {
		t.Fatalf("resume retained %d messages", len(window))
	}
	a := core.NewAgent(nil, "model", "", nil)
	a.SetMessages(window)
	a.SetWIP(true)
	if got := a.WIPState().BaselineTurn; got != 150 {
		t.Fatalf("baseline = %d, want 150", got)
	}
}

func TestWIPProviderWireFormats(t *testing.T) {
	for _, tc := range []struct {
		name      string
		newClient func(string, string) provider.Client
	}{
		{"openai", provider.NewOpenAI},
		{"responses", provider.NewOpenAIResponses},
		{"anthropic", provider.NewAnthropic},
		{"gemini", provider.NewGemini},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests <- data
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"message":"synthetic stop"}}`)
			}))
			defer server.Close()
			providerID := tc.name
			if providerID == "responses" {
				providerID = "openai"
			}
			if providerID == "gemini" {
				providerID = "google"
			}
			models := provider.ModelsForProvider(providerID)
			if len(models) == 0 {
				t.Fatal("missing test model")
			}
			a := core.NewAgent(tc.newClient("synthetic", server.URL), models[0].ID, "unchanged system", nil)
			a.SetMessages([]provider.Message{{Role: provider.RoleUser, WIP: true, Content: []provider.Content{provider.TextBlock{Text: "old draft"}}}, {Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "old answer"}}}})
			a.SetWIP(true)
			if err := a.Prompt(context.Background(), "latest text", nil, nil); err == nil {
				t.Fatal("expected synthetic HTTP error")
			} else {
				t.Logf("provider result: %v", err)
			}
			select {
			case data := <-requests:
				var wire any
				if err := json.Unmarshal(data, &wire); err != nil {
					t.Fatal(err)
				}
				var text strings.Builder
				var visit func(any)
				visit = func(v any) {
					switch x := v.(type) {
					case string:
						text.WriteString(x)
						text.WriteString("\n")
					case []any:
						for _, child := range x {
							visit(child)
						}
					case map[string]any:
						for _, child := range x {
							visit(child)
						}
					}
				}
				visit(wire)
				if strings.Count(text.String(), "<wip-mode>") != 1 || !strings.Contains(text.String(), "latest text") {
					t.Fatalf("invalid WIP wire: %s", data)
				}
				if a.System != "unchanged system" || core.MessageText(a.Messages()[2]) != "latest text" {
					t.Fatal("persistent prompt changed")
				}
			default:
				t.Fatal("no local request")
			}
		})
	}
}

func TestWIPArgsAndRPC(t *testing.T) {
	args, err := ParseArgs([]string{"--wip", "-p", "explore"})
	if err != nil || !args.WIP || args.Mode != ModePrint {
		t.Fatalf("args=%+v error=%v", args, err)
	}
	var out bytes.Buffer
	a := core.NewAgent(nil, "model", "system", nil)
	s := &rpcServer{ctx: context.Background(), agent: a, out: &out}
	s.dispatch("set_wip", "on", []byte(`{"active":true}`))
	if !a.WIPState().Active {
		t.Fatal(out.String())
	}
	if state := s.snapshotState()["wip"].(core.WIPState); !state.Active || state.Since.IsZero() {
		t.Fatal(state)
	}
	s.dispatch("set_wip", "invalid", []byte(`{}`))
	if !strings.Contains(out.String(), "active must be a boolean") {
		t.Fatal(out.String())
	}
	s.dispatch("set_wip", "off", []byte(`{"active":false}`))
	if a.WIPState().Active {
		t.Fatal("RPC disable failed")
	}
}

func TestWIPGitBaseline(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	dir := t.TempDir()
	if got := gitWIPBaseline(dir); got != "" {
		t.Fatal(got)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git failed: %v: %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init")
	run("-c", "user.name=Synthetic", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "baseline")
	if err := os.WriteFile(filepath.Join(dir, "preexisting"), []byte("user work"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "preexisting")
	a := core.NewAgent(nil, "model", "system", nil)
	configureWIP(a, Resolved{CWD: dir, WIPOn: true})
	if prompt := a.WIPDonePrompt(); !strings.Contains(prompt, "Leave pre-existing") || !strings.Contains(prompt, run("rev-parse", "HEAD")) {
		t.Fatalf("unsafe cleanup guidance: %s", prompt)
	}
	if got, want := gitWIPBaseline(dir), run("rev-parse", "HEAD"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWIPSessionBindingRestoresState(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	cwd := t.TempDir()
	r := Resolved{CWD: cwd, WIPOn: true}
	a := core.NewAgent(&startCaptureClient{}, "model", "system", nil)
	configureWIP(a, r)
	sess, err := openOrCreateSession(Args{CWD: cwd}, r, a, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Prompt(context.Background(), "draft", nil, nil); err != nil {
		t.Fatal(err)
	}
	WriteNewTranscript(a, sess, 0)
	a.SetWIP(false)
	state := a.WIPState()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	b := core.NewAgent(nil, "model", "system", nil)
	b.SetWIP(true)
	resumed, err := openOrCreateSession(Args{Session: sess.Path, CWD: cwd}, r, b, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if b.WIPState() != state || !b.Messages()[0].WIP {
		t.Fatal("resume lost state")
	}
}

func TestWIPSwarmStartupInheritance(t *testing.T) {
	t.Setenv("ZOT_SWARM_WIP_STDIN", "1")
	a := core.NewAgent(nil, "", "", nil)
	a.SetWIP(true)
	input := swarm.StartupInput{WIP: &swarm.WIPContext{State: a.WIPState(), Settings: core.WIPSettings{Instruction: "custom"}}}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	args := Args{}
	if _, err := readSwarmStartupInput(bytes.NewReader(encoded), false, false, &args); err != nil {
		t.Fatal(err)
	}
	if args.inheritedWIP == nil || args.inheritedWIP.State != a.WIPState() || args.WIPSettings.Instruction != "custom" {
		t.Fatal(fmt.Sprint(args.inheritedWIP))
	}
}
