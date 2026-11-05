package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

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
