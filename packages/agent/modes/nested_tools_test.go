package modes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

type nestedDisplayTool struct{ name string }

func (t nestedDisplayTool) Name() string          { return t.name }
func (nestedDisplayTool) Description() string     { return "Synthetic tool" }
func (nestedDisplayTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t nestedDisplayTool) Execute(ctx context.Context, _ json.RawMessage, _ func(string)) (core.ToolResult, error) {
	if t.name == "batch" {
		core.ToolRuntimeFromContext(ctx).Call(ctx, "read", json.RawMessage(`{"path":"nested.txt"}`))
		return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "filtered result"}}}, nil
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "nested result"}}}, nil
}

type nestedDisplayClient struct{}

func (nestedDisplayClient) Name() string { return "test" }
func (nestedDisplayClient) Stream(context.Context, provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 1)
	ch <- provider.EventDone{Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "final answer"}}}, Stop: provider.StopEnd}
	close(ch)
	return ch, nil
}

func TestNestedToolsRemainAfterAssistantStartsAndSessionReload(t *testing.T) {
	ag := core.NewAgent(nestedDisplayClient{}, "test", "", core.NewRegistry(nestedDisplayTool{"batch"}, nestedDisplayTool{"read"}))
	i := NewInteractive(InteractiveConfig{Agent: ag, Theme: tui.Dark})
	i.busy = true
	if err := ag.PromptWithTool(context.Background(), "run", provider.ToolCallBlock{ID: "parent", Name: "batch", Arguments: json.RawMessage(`{}`)}, "test", i.handleEvent); err != nil {
		t.Fatal(err)
	}
	for _, busy := range []bool{true, false} {
		i.busy = busy
		assertNestedDisplay(t, i)
	}
	session, err := core.NewSession(t.TempDir(), "/workspace", "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range ag.Messages() {
		if err := session.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, messages, err := core.OpenSession(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	ag.SetMessages(messages)
	i = NewInteractive(InteractiveConfig{Agent: ag, Theme: tui.Dark})
	assertNestedDisplay(t, i)
}

func assertNestedDisplay(t *testing.T, i *Interactive) {
	t.Helper()
	chat := strings.Join(i.buildChatLocked(100), "\n")
	if strings.Count(chat, "nested.txt") != 1 || strings.Count(chat, "nested result") != 1 || !strings.Contains(chat, "filtered result") {
		t.Fatalf("nested call disappeared or duplicated:\n%s", chat)
	}
	if strings.Index(chat, "nested.txt") > strings.Index(chat, "final answer") {
		t.Fatalf("nested call appeared after the final answer:\n%s", chat)
	}
}
