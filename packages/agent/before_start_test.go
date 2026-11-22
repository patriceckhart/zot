package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestStartExtensionProcessHelper(t *testing.T) {
	if os.Getenv("ZOT_TEST_START_EXTENSION") != "1" {
		return
	}
	enc := json.NewEncoder(os.Stdout)
	enc.Encode(map[string]any{"type": "hello", "name": "start-test", "version": "1"})
	enc.Encode(extproto.SubscribeFromExt{Type: "subscribe", Intercept: []string{"before_agent_start"}})
	enc.Encode(extproto.ReadyFromExt{Type: "ready"})
	scanner := bufio.NewScanner(os.Stdin)
	count := 0
	for scanner.Scan() {
		var ev extproto.EventInterceptFromHost
		if json.Unmarshal(scanner.Bytes(), &ev) != nil {
			os.Exit(2)
		}
		if ev.Type == "shutdown" {
			os.Exit(0)
		}
		if ev.Event != "before_agent_start" {
			continue
		}
		if ev.SystemPrompt == nil || ev.CWD == "" || ev.Provider == "" || ev.Model == "" || ev.SessionID == "" || ev.AgentRunID == "" {
			os.Exit(3)
		}
		wantAlias := ""
		if ev.SessionID == "named-session" {
			wantAlias = "tidy-birch"
		}
		if ev.SessionAlias != wantAlias {
			os.Exit(4)
		}
		count++
		value, _ := json.Marshal(fmt.Sprintf("%s/%d/%s", *ev.SystemPrompt, count, ev.Model))
		enc.Encode(extproto.EventInterceptResponseFromExt{Type: "event_intercept_response", ID: ev.ID, SystemPrompt: value})
	}
	os.Exit(0)
}

type startCaptureClient struct{ requests []provider.Request }

func (*startCaptureClient) Name() string { return "test" }
func (c *startCaptureClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.requests = append(c.requests, req)
	ch := make(chan provider.Event, 1)
	ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "ok"}}}}
	close(ch)
	return ch, nil
}

func TestRPCBeforeAgentStart(t *testing.T) {
	t.Setenv("ZOT_TEST_START_EXTENSION", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	manifest, _ := json.Marshal(map[string]any{"name": "start-test", "version": "1", "exec": exe, "args": []string{"-test.run=^TestStartExtensionProcessHelper$"}, "enabled": true})
	if err := os.WriteFile(filepath.Join(dir, "extension.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	models := provider.ModelsForProvider("anthropic")
	if len(models) < 2 {
		t.Fatal("need two catalog models")
	}
	m := extensions.New(t.TempDir(), dir, "test", "anthropic", models[0].ID, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer m.Stop(time.Second)
	if errs := m.LoadExplicit(ctx, []string{dir}); len(errs) > 0 {
		t.Fatal(errs)
	}
	m.WaitForReady(time.Second)
	client := &startCaptureClient{}
	ag := core.NewAgent(client, models[0].ID, "complete base", nil)
	wireNonInteractiveAgentExtHooks(ctx, ag, m)
	var out bytes.Buffer
	server := &rpcServer{agent: ag, provider: "anthropic", model: ag.Model, out: &out}
	prompt := func(want string) {
		t.Helper()
		if err := ag.Prompt(ctx, "hello", nil, nil); err != nil {
			t.Fatal(err)
		}
		if got := client.requests[len(client.requests)-1].System; got != want {
			t.Fatalf("request system=%q want %q", got, want)
		}
	}
	prompt("complete base/1/" + models[0].ID)
	prompt("complete base/1/" + models[0].ID)
	server.dispatch("clear", "1", nil)
	prompt("complete base/2/" + models[0].ID)
	raw, _ := json.Marshal(map[string]string{"model": models[1].ID})
	server.dispatch("set_model", "2", raw)
	prompt("complete base/3/" + models[1].ID)
	prompt("complete base/3/" + models[1].ID)
	bindAgentSession(ag, &core.Session{ID: "named-session", Meta: core.SessionMeta{Alias: "tidy-birch"}})
	prompt("complete base/4/" + models[1].ID)
	bindAgentSession(ag, &core.Session{ID: "legacy-session"})
	prompt("complete base/5/" + models[1].ID)
}
