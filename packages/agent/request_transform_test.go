package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/ext"
	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestRequestTransformProcessHelper(t *testing.T) {
	if os.Getenv("ZOT_TEST_REQUEST_TRANSFORM") != "1" {
		return
	}
	e := ext.New("transform-test", "1")
	e.InterceptBeforeRequest(ext.RequestTransformOptions{}, func(ctx context.Context, event ext.RequestTransformEvent) (map[string]json.RawMessage, error) {
		out := make(map[string]json.RawMessage)
		for _, s := range event.Segments {
			if s.Skipped != "" {
				continue
			}
			if s.Kind == "text" {
				out[s.ID], _ = json.Marshal(strings.ReplaceAll(s.Text, "SECRET", "CLEAN"))
			}
			if s.Kind == "json" {
				out[s.ID] = json.RawMessage(strings.ReplaceAll(string(s.Value), "SECRET", "CLEAN"))
			}
		}
		return out, nil
	})
	if err := e.Run(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

type transformCaptureClient struct{ requests []provider.Request }

func (*transformCaptureClient) Name() string { return "test" }
func (c *transformCaptureClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.requests = append(c.requests, req)
	ch := make(chan provider.Event, 2)
	ch <- provider.EventTextDelta{Delta: "summary"}
	ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "summary"}}}}
	close(ch)
	return ch, nil
}

func TestRequestTransformModeWiring(t *testing.T) {
	t.Setenv("ZOT_TEST_REQUEST_TRANSFORM", "1")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	manifest, _ := json.Marshal(map[string]any{"name": "transform-test", "exec": exe, "args": []string{"-test.run=^TestRequestTransformProcessHelper$"}, "request_transform": map[string]any{}})
	if err := os.WriteFile(filepath.Join(dir, "extension.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	mgr := extensions.New(t.TempDir(), dir, "test", "test", "model", nil)
	defer mgr.Stop(time.Second)
	if errs := mgr.LoadExplicit(ctx, []string{dir}); len(errs) > 0 {
		t.Fatal(errs)
	}
	mgr.WaitForReady(time.Second)
	for _, mode := range []string{"interactive", "non-interactive", "continuous", "large-escaped-frame"} {
		t.Run(mode, func(t *testing.T) {
			client := &transformCaptureClient{}
			a := core.NewAgent(client, "model", "system SECRET", nil)
			if mode == "continuous" {
				resolved := Resolved{Provider: "openai", Model: "gpt-4o-mini", Credential: "synthetic-key", SystemPrompt: "system SECRET"}
				engine := newContinuousEngine(resolved, mgr, nil, func() *continuous.Service { return nil })
				var err error
				a, err = engine.Build(ctx, continuous.Conversation{})
				if err != nil {
					t.Fatal(err)
				}
				a.Client = client
			} else if mode == "interactive" {
				wireBeforeAgentStart(a, mgr, "test")
			} else {
				wireNonInteractiveAgentExtLifecycle(ctx, a, mgr)
			}
			text := "message SECRET"
			if mode == "large-escaped-frame" {
				text += strings.Repeat("\x00", 3<<20)
			}
			if err := a.Prompt(ctx, text, nil, nil); err != nil {
				t.Fatal(err)
			}
			if len(client.requests) != 1 || client.requests[0].System != "system CLEAN" {
				t.Fatal("mode sent untransformed system")
			}
			if got := client.requests[0].Messages[0].Content[0].(provider.TextBlock).Text; strings.Contains(got, "SECRET") || len(got) != len(text)-1 {
				t.Fatal("message not transformed")
			}
			if got := a.Messages()[0].Content[0].(provider.TextBlock).Text; got != text {
				t.Fatal("local transcript changed")
			}
			for _, compactionMode := range []string{"side", "in_place"} {
				a.CompactionMode = compactionMode
				if _, _, err := a.Summarize(ctx, a.Messages(), "SECRET", nil); err != nil {
					t.Fatal(err)
				}
				last := client.requests[len(client.requests)-1]
				segments, err := core.RequestSegments(last)
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range segments {
					if strings.Contains(s.Text, "SECRET") {
						t.Fatal("compaction bypassed transform")
					}
				}
			}
		})
	}
	// Failed reloads must not silently remove a previously active requirement.
	broken, _ := json.Marshal(map[string]any{"name": "transform-test", "exec": filepath.Join(dir, "missing")})
	if err := os.WriteFile(filepath.Join(dir, "extension.json"), broken, 0600); err != nil {
		t.Fatal(err)
	}
	if stats := mgr.Reload(ctx, time.Second); len(stats.Errors) == 0 {
		t.Fatal("expected failed reload")
	}
	if _, err := mgr.TransformRequest(ctx, "session", "test", "chat", provider.Request{}); err == nil {
		t.Fatal("failed reload silently disabled transform")
	}

	// Startup failures must be enforceable before a capability can be received.
	failed := extensions.New(t.TempDir(), dir, "test", "test", "model", nil)
	badDir := t.TempDir()
	bad, _ := json.Marshal(map[string]any{"name": "missing", "exec": filepath.Join(badDir, "missing"), "request_transform": extproto.RequestTransformOptions{}})
	if err := os.WriteFile(filepath.Join(badDir, "extension.json"), bad, 0600); err != nil {
		t.Fatal(err)
	}
	if errs := failed.LoadExplicit(ctx, []string{badDir}); len(errs) == 0 {
		t.Fatal("expected startup failure")
	}
	if _, err := failed.TransformRequest(ctx, "session", "test", "chat", provider.Request{}); err == nil {
		t.Fatal("startup failure silently disabled transform")
	}
}
