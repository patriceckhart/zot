package modes

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type attachedExecutionTool struct{ calls atomic.Int32 }

func (*attachedExecutionTool) Name() string            { return "effect" }
func (*attachedExecutionTool) Description() string     { return "test effect" }
func (*attachedExecutionTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *attachedExecutionTool) Execute(context.Context, json.RawMessage, func(string)) (core.ToolResult, error) {
	t.calls.Add(1)
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "effect"}}}, nil
}

func attachedExecutionHistory() []provider.Message {
	var messages []provider.Message
	for _, text := range []string{"one", "two", "three", "four", "five", "six"} {
		messages = append(messages, provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: text}}})
	}
	return messages
}

func waitAttachedIdle(t *testing.T, iv *Interactive, requireError bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		iv.mu.Lock()
		idle := !iv.busy && !iv.compacting && (!requireError || iv.statusErr != "")
		iv.mu.Unlock()
		if idle {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("attached action did not settle")
		}
		runtime.Gosched()
	}
}

func TestAttachedModeRejectsLocalExecution(t *testing.T) {
	for _, action := range []string{"compact command", "automatic compact", "extension response", "tool prelude", "local continuation"} {
		t.Run(action, func(t *testing.T) {
			client := &toolPromptClient{requests: make(chan provider.Request, 8)}
			tool := &attachedExecutionTool{}
			agent := core.NewAgent(client, "test-model", "", core.NewRegistry(tool))
			history := attachedExecutionHistory()
			agent.SetMessages(history)
			var submitted atomic.Int32
			iv := NewInteractive(InteractiveConfig{Agent: agent, Extensions: &extensions.Manager{}, PromptDriver: func(context.Context, *core.Agent, string, func(core.AgentEvent)) error {
				submitted.Add(1)
				return nil
			}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			iv.runCtx = ctx
			call := provider.ToolCallBlock{ID: "c1", Name: "effect", Arguments: json.RawMessage(`{}`)}
			switch action {
			case "compact command":
				iv.runSlash(ctx, "/compact")
			case "automatic compact":
				iv.runCompact(ctx, true)
			case "extension response":
				iv.applyExtensionCommandResponse("shortcut", extproto.CommandResponseFromExt{Action: "tool_prompt", Prompt: "do it", ToolName: "effect", ToolArgs: json.RawMessage(`{}`)})
			case "tool prelude":
				iv.startTurnWithPrelude(ctx, "do it", nil, &toolPromptRequest{call: call, origin: "shortcut"})
			case "local continuation":
				iv.startTurnRequest(ctx, "", nil, true)
			}
			waitAttachedIdle(t, iv, false)
			if len(client.requests) != 0 || tool.calls.Load() != 0 || submitted.Load() != 0 {
				t.Fatalf("action escaped attached execution: requests=%d tools=%d submissions=%d", len(client.requests), tool.calls.Load(), submitted.Load())
			}
			if !reflect.DeepEqual(agent.Messages(), history) {
				t.Fatal("unsupported action changed the mirrored transcript")
			}
			iv.mu.Lock()
			status := iv.statusErr
			iv.mu.Unlock()
			if !strings.Contains(status, "attached") {
				t.Fatalf("missing attached-mode explanation: %q", status)
			}
		})
	}
}

func TestAttachedModeLeavesAutomaticCompactionToHost(t *testing.T) {
	agent := core.NewAgent(nil, "test-model", "", nil)
	iv := NewInteractive(InteractiveConfig{Agent: agent, Provider: "anthropic", Model: "claude-sonnet-4-5", PromptDriver: func(context.Context, *core.Agent, string, func(core.AgentEvent)) error { return nil }})
	iv.lastCtxInput = 200000
	iv.mu.Lock()
	compact := iv.shouldAutoCompactLocked()
	iv.mu.Unlock()
	if compact {
		t.Fatal("attached context measurement enabled local compaction")
	}
	if err := iv.compactBetweenTurns(context.Background()); err != nil {
		t.Fatalf("attached boundary compaction: %v", err)
	}
}

func TestAttachedContextOverflowDoesNotRecoverLocally(t *testing.T) {
	client := &toolPromptClient{requests: make(chan provider.Request, 8)}
	agent := core.NewAgent(client, "test-model", "", nil)
	history := attachedExecutionHistory()
	agent.SetMessages(history)
	overflow := errors.New("context window exceeded on the host")
	var submissions atomic.Int32
	iv := NewInteractive(InteractiveConfig{Agent: agent, PromptDriver: func(context.Context, *core.Agent, string, func(core.AgentEvent)) error {
		submissions.Add(1)
		return overflow
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	iv.runCtx = ctx
	iv.startTurn(ctx, "hello")
	waitAttachedIdle(t, iv, true)
	if len(client.requests) != 0 || submissions.Load() != 1 {
		t.Fatalf("host failure triggered local recovery: requests=%d submissions=%d", len(client.requests), submissions.Load())
	}
	iv.mu.Lock()
	status, pending := iv.statusErr, iv.continueAfterCompact
	iv.mu.Unlock()
	if status != overflow.Error() || pending {
		t.Fatalf("host error hidden by local recovery: status=%q pending=%v", status, pending)
	}
	if !reflect.DeepEqual(agent.Messages(), history) {
		t.Fatal("host failure changed the mirrored transcript locally")
	}
}

func TestAttachedPromptsUseHostDespiteLocalContextThreshold(t *testing.T) {
	for _, localCredential := range []bool{false, true} {
		name := "without local credential"
		if localCredential {
			name = "with local credential"
		}
		t.Run(name, func(t *testing.T) {
			client := &toolPromptClient{requests: make(chan provider.Request, 8)}
			var localClient provider.Client
			if localCredential {
				localClient = client
			}
			agent := core.NewAgent(localClient, "test-model", "", nil)
			agent.SetMessages(attachedExecutionHistory())
			var submissions atomic.Int32
			iv := NewInteractive(InteractiveConfig{Agent: agent, Provider: "anthropic", Model: "claude-sonnet-4-5", PromptDriver: func(_ context.Context, _ *core.Agent, prompt string, sink func(core.AgentEvent)) error {
				if prompt != "hello" {
					t.Errorf("host prompt=%q", prompt)
				}
				submissions.Add(1)
				sink(core.EvUsage{Usage: provider.Usage{InputTokens: 200000}})
				return nil
			}})
			iv.lastCtxInput = 200000
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			iv.runCtx = ctx
			// Plain extension prompts remain supported and go to the host.
			iv.applyExtensionCommandResponse("shortcut", extproto.CommandResponseFromExt{Action: "prompt", Prompt: "hello"})
			waitAttachedIdle(t, iv, false)
			if submissions.Load() != 1 || len(client.requests) != 0 {
				t.Fatalf("prompt left the host path: submissions=%d requests=%d", submissions.Load(), len(client.requests))
			}
			iv.mu.Lock()
			status, compacting := iv.statusErr, iv.compacting
			iv.mu.Unlock()
			if status != "" || compacting {
				t.Fatalf("client attempted compaction after host usage: status=%q compacting=%v", status, compacting)
			}
		})
	}
}

func TestAttachedUnsupportedActionsWithoutLocalCredentials(t *testing.T) {
	agent := core.NewAgent(nil, "test-model", "", nil)
	history := attachedExecutionHistory()
	agent.SetMessages(history)
	iv := NewInteractive(InteractiveConfig{Agent: agent, PromptDriver: func(context.Context, *core.Agent, string, func(core.AgentEvent)) error {
		t.Error("unsupported action submitted host work")
		return nil
	}})
	iv.runSlash(context.Background(), "/compact")
	if !strings.Contains(iv.statusErr, "conversation.compact") || iv.busy {
		t.Fatalf("compact refusal=%q busy=%v", iv.statusErr, iv.busy)
	}
	// No extension manager is needed to reject this action safely.
	iv.applyExtensionCommandResponse("shortcut", extproto.CommandResponseFromExt{Action: "tool_prompt", Prompt: "do it", ToolName: "effect", ToolArgs: json.RawMessage(`{}`)})
	if !strings.Contains(iv.statusErr, "tool_prompt") || iv.busy {
		t.Fatalf("tool_prompt refusal=%q busy=%v", iv.statusErr, iv.busy)
	}
	if !reflect.DeepEqual(agent.Messages(), history) {
		t.Fatal("unsupported action changed the credential-less mirror")
	}
}
