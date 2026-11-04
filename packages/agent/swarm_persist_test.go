package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type swarmPersistClient struct{}

func (swarmPersistClient) Name() string { return "synthetic" }
func (swarmPersistClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 1)
	msg := provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "done"}}}
	stop := provider.StopEnd
	if len(req.Messages) == 1 {
		msg.Content = []provider.Content{provider.ToolCallBlock{ID: "call", Name: "blocking", Arguments: json.RawMessage(`{"command":"synthetic"}`)}}
		stop = provider.StopToolUse
	}
	ch <- provider.EventDone{Message: msg, Stop: stop}
	close(ch)
	return ch, nil
}

type swarmPersistTool struct{ started, release chan struct{} }

func (swarmPersistTool) Name() string            { return "blocking" }
func (swarmPersistTool) Description() string     { return "Synthetic tool" }
func (swarmPersistTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t swarmPersistTool) Execute(ctx context.Context, _ json.RawMessage, _ func(string)) (core.ToolResult, error) {
	close(t.started)
	select {
	case <-t.release:
	case <-ctx.Done():
		return core.ToolResult{}, ctx.Err()
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "synthetic result"}}}, nil
}

func TestSwarmPersistsCallsBeforeToolCompletes(t *testing.T) {
	tool := swarmPersistTool{make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ag := core.NewAgent(swarmPersistClient{}, "test", "", core.NewRegistry(tool))
	sess, err := core.NewSession(t.TempDir(), t.TempDir(), "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	persistSwarmMessages(ag, sess)
	done := make(chan struct{})
	var runErr error
	go func() { defer close(done); runErr = ag.Prompt(ctx, "run", nil, func(core.AgentEvent) {}) }()
	defer func() { cancel(); <-done; sess.Close() }()
	select {
	case <-tool.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	raw, err := os.ReadFile(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"name":"blocking"`) {
		t.Fatalf("call not persisted while tool running: %s", raw)
	}
	close(tool.release)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	raw, err = os.ReadFile(sess.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"name":"blocking"`) != 1 || strings.Count(string(raw), "synthetic result") != 1 {
		t.Fatalf("missing or duplicate messages: %s", raw)
	}
}
