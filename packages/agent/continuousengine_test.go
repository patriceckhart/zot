package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
)

func TestContinuousEngineIsolatesRegistries(t *testing.T) {
	r := Resolved{Provider: "openai", Credential: "synthetic-key", Model: "gpt-4o-mini", ToolRegistry: core.NewRegistry()}
	engine := newContinuousEngine(r, nil, nil, func() *continuous.Service { return nil })
	agents := make(chan *core.Agent, 32)
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for i := 0; i < cap(agents); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ag, err := engine.Build(context.Background(), continuous.Conversation{})
			if err != nil {
				errs <- err
				return
			}
			agents <- ag
		}()
	}
	wg.Wait()
	close(agents)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if len(r.ToolRegistry) != 0 {
		t.Fatalf("host registry mutated: %v", r.ToolRegistry)
	}
	for ag := range agents {
		if len(ag.Tools) != 2 {
			t.Fatalf("agent registry changed by another build: %v", ag.Tools)
		}
		delete(ag.Tools, "handoff")
	}
}

func TestContinuousEngineRejectsProviderMismatch(t *testing.T) {
	r := Resolved{Provider: "openai", Credential: "synthetic-key", Model: "gpt-4o-mini"}
	engine := newContinuousEngine(r, nil, nil, func() *continuous.Service { return nil })
	for _, name := range []string{"", "openai", "OpenAI"} {
		if _, err := engine.Build(context.Background(), continuous.Conversation{Config: continuous.AgentConfig{Provider: name}}); err != nil {
			t.Fatalf("provider %q: %v", name, err)
		}
	}
	if _, err := engine.Build(context.Background(), continuous.Conversation{Config: continuous.AgentConfig{Provider: "anthropic", Model: "claude-test"}}); err == nil || !strings.Contains(err.Error(), "does not match host provider") {
		t.Fatalf("mismatched provider accepted: %v", err)
	}
}

func TestContinuousExtensionGenerationsRemainIndependent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires /bin/sh")
	}
	t.Setenv("ZOT_HOME", t.TempDir())
	dir := t.TempDir()
	writeGeneration := func(version string) {
		t.Helper()
		script := fmt.Sprintf(`#!/bin/sh
response='%s'
printf '%%s\n' '{"type":"hello","name":"generation","version":"1","capabilities":["tools"]}'
printf '%%s\n' '{"type":"register_tool","name":"generation","description":"generation fixture","schema":{"type":"object"}}'
printf '%%s\n' '{"type":"ready"}'
while IFS= read -r line
do
  if printf '%%s' "$line" | grep -q '"type":"tool_call"'
  then
    id=$(printf '%%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
    printf '%%s\n' "{\"type\":\"tool_result\",\"id\":\"$id\",\"content\":[{\"type\":\"text\",\"text\":\"$response\"}]}"
  elif printf '%%s' "$line" | grep -q '"type":"shutdown"'
  then
    printf '%%s\n' '{"type":"shutdown_ack"}'
    exit 0
  fi
done
`, version)
		if err := os.WriteFile(filepath.Join(dir, "run.sh"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "extension.json"), []byte(`{"name":"generation","exec":"./run.sh"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args := Args{Mode: ModePrint, Provider: "openai", Model: "gpt-4o-mini", APIKey: "synthetic-key", CWD: t.TempDir(), NoExt: true, NoSkill: true, Exts: []string{dir}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	service := func() *continuous.Service { return nil }
	writeGeneration("old")
	old, stopOld, err := loadContinuousEngine(ctx, args, nil, service)
	if err != nil {
		t.Fatal(err)
	}
	defer stopOld()
	oldAgent, err := old.Build(ctx, continuous.Conversation{})
	if err != nil {
		t.Fatal(err)
	}
	writeGeneration("new")
	next, stopNext, err := loadContinuousEngine(ctx, args, nil, service)
	if err != nil {
		t.Fatal(err)
	}
	defer stopNext()
	// A bad replacement must not disturb either previously loaded manager.
	if err := os.WriteFile(filepath.Join(dir, "extension.json"), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}
	if engine, stop, err := loadContinuousEngine(ctx, args, nil, service); err == nil {
		stop()
		t.Fatalf("invalid replacement accepted: %v", engine)
	}
	nextAgent, err := next.Build(ctx, continuous.Conversation{})
	if err != nil {
		t.Fatal(err)
	}
	for version, ag := range map[string]*core.Agent{"old": oldAgent, "new": nextAgent} {
		tool, err := ag.Tools.Get("generation")
		if err != nil {
			t.Fatal(err)
		}
		result, err := tool.Execute(ctx, json.RawMessage(`{}`), func(string) {})
		if err != nil || result.IsError || core.ToolResultText(result) != version {
			t.Fatalf("%s generation changed or stopped: result=%+v err=%v", version, result, err)
		}
	}
}
