// An idempotent external action has several attempts but one effect, and a
// reconciling tool is asked whether an interrupted operation completed before
// anything is repeated. Both run through recovery after a simulated crash.
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/patriceckhart/zot/examples/continuous/internal/synthetic"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// receiver stands in for an external service that deduplicates on the
// operation key the host supplies.
type receiver struct {
	effects map[string]int
	block   chan struct{}
	started chan struct{}
}

func (r *receiver) Name() string                        { return "send_email" }
func (r *receiver) Description() string                 { return "send an email through an idempotent receiver" }
func (r *receiver) Schema() json.RawMessage             { return json.RawMessage(`{"type":"object"}`) }
func (r *receiver) ReplayPolicy() core.ToolReplayPolicy { return core.ReplayIdempotent }
func (r *receiver) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	key := core.ToolOperationKey(ctx)
	r.effects[key]++ // the receiver would ignore a repeated key
	if r.started != nil {
		r.started <- struct{}{}
	}
	if r.block != nil {
		select {
		case <-r.block:
		case <-ctx.Done():
			return core.ToolResult{}, ctx.Err()
		}
	}
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "sent (operation " + key + ")"}}}, nil
}

func main() {
	ctx := context.Background()
	rt, err := synthetic.OpenStore(ctx, synthetic.TempStore("idempotent"))
	synthetic.Must(err)
	defer rt.Close()
	tool := &receiver{effects: map[string]int{}, block: make(chan struct{}), started: make(chan struct{}, 1)}
	client := synthetic.NewClient(
		synthetic.Step{Calls: []provider.ToolCallBlock{synthetic.Call("c1", "send_email", `{"to":"ops@example.test"}`)}},
		synthetic.Step{Text: "Email sent."},
	)
	svc, err := continuous.NewService(rt, synthetic.Engine(client, tool), continuous.ExecutionOptions{})
	synthetic.Must(err)
	c, err := rt.OpenRoot(ctx, "mail", continuous.AgentConfig{Provider: "synthetic", Model: "synthetic-model"})
	synthetic.Must(err)
	_, err = rt.Submit(ctx, c.ID, "user", "", "Send the report.")
	synthetic.Must(err)

	// Interrupt while the tool is running: the intent is committed, the
	// result is not. Recovery decides by the replay contract.
	stepCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, _, err := svc.Step(stepCtx, c.ID); done <- err }()
	<-tool.started
	cancel()
	<-done
	plan, _ := rt.RecoveryPreview(ctx)
	fmt.Println("recovery:", plan.Actions[0].Action, "-", plan.Actions[0].Detail)

	tool.block = nil
	run, _, err := svc.Step(ctx, c.ID)
	synthetic.Must(err)
	keys := 0
	deliveries := 0
	for _, n := range tool.effects {
		keys++
		deliveries += n
	}
	fmt.Printf("run %s: %d deliveries under %d operation key(s)\n", run.Outcome, deliveries, keys)
	for _, n := range run.Notices {
		fmt.Println("  notice:", n)
	}
}
