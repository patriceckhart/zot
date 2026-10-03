package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestCodemodeToolSelection(t *testing.T) {
	cwd := t.TempDir()
	if buildToolRegistry(Args{}, cwd, nil)["codemode"] != nil {
		t.Fatal("codemode must be opt-in")
	}
	if len(buildToolRegistry(Args{NoTools: true, Tools: []string{"codemode"}}, cwd, nil)) != 0 {
		t.Fatal("no-tools must take precedence")
	}
	reg := buildToolRegistry(Args{Tools: []string{"read", "codemode"}}, cwd, nil)
	if len(reg) != 2 || reg["read"] == nil || reg["codemode"] == nil {
		t.Fatalf("registry: %v", reg)
	}
	summaries := toolSummaries(reg, Args{})
	if len(summaries) != 2 || summaries[1].Name != "codemode" {
		t.Fatalf("summaries: %+v", summaries)
	}
	args, err := ParseArgs([]string{"--tools", "read,codemode"})
	if err != nil {
		t.Fatal(err)
	}
	if buildToolRegistry(args, cwd, nil)["codemode"] == nil {
		t.Fatal("CLI selection lost codemode")
	}
}

type codemodeClient struct{ requests []provider.Request }

func (*codemodeClient) Name() string { return "test" }
func (c *codemodeClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.requests = append(c.requests, req)
	message := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}
	stop := provider.StopEnd
	if len(c.requests) == 1 {
		message.Content = []provider.Content{provider.ToolCallBlock{ID: "script", Name: "codemode", Arguments: json.RawMessage(`{"code":"const source = await tools.read({path:'package.json'}); return JSON.parse(source).name"}`)}}
		stop = provider.StopToolUse
	}
	events := make(chan provider.Event, 1)
	events <- provider.EventDone{Stop: stop, Message: message}
	close(events)
	return events, nil
}

func TestCodemodeAgentLoopAndSession(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "package.json"), []byte(`{"name":"kept","large":"discarded intermediate text"}`), 0600); err != nil {
		t.Fatal(err)
	}
	client := &codemodeClient{}
	ag := core.NewAgent(client, "test", "", buildToolRegistry(Args{Tools: []string{"read", "codemode"}}, cwd, nil))
	session, err := core.NewSession(t.TempDir(), cwd, "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	ag.OnMessageAppended = func(msg provider.Message) {
		if err := session.AppendMessage(msg); err != nil {
			t.Error(err)
		}
	}
	var calls, results []string
	if err := ag.Prompt(context.Background(), "read package name", nil, func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvToolCall:
			calls = append(calls, e.ID)
		case core.EvToolResult:
			results = append(results, e.ID)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 2 || len(calls) != 2 || len(results) != 2 || results[0] != "script/1" || results[1] != "script" {
		t.Fatalf("requests=%d calls=%v results=%v", len(client.requests), calls, results)
	}
	messages := client.requests[1].Messages
	encoded, _ := json.Marshal(messages)
	if strings.Contains(string(encoded), "discarded intermediate text") {
		t.Fatalf("intermediate result in model context: %s", encoded)
	}
	if len(messages) != 3 {
		t.Fatalf("messages: %+v", messages)
	}
	call := messages[1].Content[0].(provider.ToolCallBlock)
	result := messages[2].Content[0].(provider.ToolResultBlock)
	var output strings.Builder
	for _, block := range result.Content {
		if text, ok := block.(provider.TextBlock); ok {
			output.WriteString(text.Text)
		}
	}
	if call.Name != "codemode" || result.CallID != call.ID || result.IsError || len(messages[2].Content) != 1 || !strings.Contains(output.String(), "kept") {
		t.Fatalf("invalid outer pairing: %+v", messages)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, replayed, err := core.OpenSession(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if len(replayed) != 4 || replayed[2].Content[0].(provider.ToolResultBlock).CallID != "script" {
		t.Fatalf("replay: %+v", replayed)
	}
}
