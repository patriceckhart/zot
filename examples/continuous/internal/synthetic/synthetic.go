// Package synthetic provides a scripted provider and simple tools so the
// continuous examples run without credentials or paid model calls.
package synthetic

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Step is one scripted model response: text, tool calls, or an error.
type Step struct {
	Text  string
	Calls []provider.ToolCallBlock
	Err   error
}

// Call builds a tool call block.
func Call(id, name, args string) provider.ToolCallBlock {
	return provider.ToolCallBlock{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// Client answers requests from a script, in order. When the script runs out
// it echoes the last user message so examples stay deterministic.
type Client struct {
	mu    sync.Mutex
	steps []Step
}

func NewClient(steps ...Step) *Client { return &Client{steps: steps} }

func (c *Client) Name() string { return "synthetic" }

func (c *Client) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.mu.Lock()
	var step Step
	if len(c.steps) > 0 {
		step, c.steps = c.steps[0], c.steps[1:]
	} else if len(req.Messages) > 0 {
		step = Step{Text: "echo: " + core.MessageText(req.Messages[len(req.Messages)-1])}
	}
	c.mu.Unlock()
	ch := make(chan provider.Event, 8)
	go func() {
		defer close(ch)
		ch <- provider.EventStart{Model: req.Model, Provider: "synthetic"}
		if step.Err != nil {
			ch <- provider.EventDone{Stop: provider.StopError, Err: step.Err}
			return
		}
		msg := provider.Message{Role: provider.RoleAssistant, Time: time.Now().UTC()}
		if step.Text != "" {
			ch <- provider.EventTextDelta{Delta: step.Text}
			msg.Content = append(msg.Content, provider.TextBlock{Text: step.Text})
		}
		stop := provider.StopEnd
		for _, call := range step.Calls {
			msg.Content = append(msg.Content, call)
			stop = provider.StopToolUse
		}
		ch <- provider.EventUsage{Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}}
		ch <- provider.EventDone{Stop: stop, Message: msg}
	}()
	return ch, nil
}

// Engine builds agents with the client and tools for every request.
func Engine(client provider.Client, tools ...core.Tool) continuous.Engine {
	return continuous.EngineFunc(func(ctx context.Context, c continuous.Conversation) (*core.Agent, error) {
		a := core.NewAgent(client, "synthetic-model", "You are a synthetic example agent.", core.NewRegistry(tools...))
		a.MaxRetries = 0
		return a, nil
	})
}

// OpenStore opens (or creates) a journal store under dir with process
// durability, which is enough for examples and works on every platform.
func OpenStore(ctx context.Context, dir string) (*continuous.Runtime, error) {
	store, err := journal.Open(ctx, dir, journal.Options{Durability: storage.Process})
	if err != nil {
		return nil, err
	}
	return continuous.New(store)
}

// TempStore returns a fresh store path in a temporary directory.
func TempStore(name string) string {
	dir, err := os.MkdirTemp("", "zot-continuous-"+name+"-")
	if err != nil {
		panic(err)
	}
	return filepath.Join(dir, "store")
}

// Must exits with the error when it is not nil.
func Must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// EchoTool returns its arguments. It declares the replay policy it is given.
type EchoTool struct {
	ToolName string
	Replay   core.ToolReplayPolicy
	Calls    int
}

func (t *EchoTool) Name() string            { return t.ToolName }
func (t *EchoTool) Description() string     { return "synthetic tool" }
func (t *EchoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *EchoTool) ReplayPolicy() core.ToolReplayPolicy {
	if t.Replay == "" {
		return core.ReplayNever
	}
	return t.Replay
}
func (t *EchoTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	t.Calls++
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: t.ToolName + " ran with " + string(args)}}}, nil
}
