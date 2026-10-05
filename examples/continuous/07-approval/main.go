// Persisted human approval: a tool call parks the run until a decision is
// recorded. The pending record survives a restart and is never implicitly
// approved. The decision is bound to the exact arguments.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/patriceckhart/zot/examples/continuous/internal/synthetic"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/provider"
)

func main() {
	ctx := context.Background()
	store := synthetic.TempStore("approval")
	deploy := &synthetic.EchoTool{ToolName: "deploy"}
	client := synthetic.NewClient(
		synthetic.Step{Calls: []provider.ToolCallBlock{synthetic.Call("c1", "deploy", `{"env":"production"}`)}},
		synthetic.Step{Text: "Deployed to production."},
	)
	approver := func(ctx context.Context, c continuous.Conversation, call provider.ToolCallBlock, args json.RawMessage) (continuous.ApprovalRequest, bool) {
		if call.Name == "deploy" {
			return continuous.ApprovalRequest{Summary: "deploy with " + string(args)}, true
		}
		return continuous.ApprovalRequest{}, false
	}
	open := func() (*continuous.Runtime, *continuous.Service) {
		rt, err := synthetic.OpenStore(ctx, store)
		synthetic.Must(err)
		svc, err := continuous.NewService(rt, synthetic.Engine(client, deploy), continuous.ExecutionOptions{Approver: approver})
		synthetic.Must(err)
		return rt, svc
	}

	rt, svc := open()
	c, err := rt.OpenRoot(ctx, "ops", continuous.AgentConfig{Provider: "synthetic", Model: "synthetic-model"})
	synthetic.Must(err)
	_, err = rt.Submit(ctx, c.ID, "operator", "", "Deploy to production.")
	synthetic.Must(err)
	_, _, err = svc.Step(ctx, c.ID)
	fmt.Println("step without decision:", err, "| deploy calls:", deploy.Calls)
	pending, _ := rt.PendingApprovals(ctx, c.ID)
	fmt.Printf("pending approval %s: %s\n", pending[0].ID[:8], pending[0].Summary)
	synthetic.Must(rt.Close())

	// Restart: the same record is still pending, nothing ran.
	rt, svc = open()
	defer rt.Close()
	_, _, err = svc.Step(ctx, c.ID)
	fmt.Println("step after restart:", errors.Is(err, continuous.ErrAwaitingApproval), "| deploy calls:", deploy.Calls)
	plan, _ := rt.RecoveryPreview(ctx)
	fmt.Println("recovery plan:", plan.Actions[0].Action, "-", plan.Actions[0].Detail)

	// A human decides. The decision records actor and reason.
	decided, err := rt.Decide(ctx, pending[0].ID, "alice", true, "call", "change window approved")
	synthetic.Must(err)
	fmt.Printf("decision: %s by %s (%s)\n", decided.State, decided.Actor, decided.Reason)
	run, _, err := svc.Step(ctx, c.ID)
	synthetic.Must(err)
	fmt.Println("run:", run.Outcome, "| deploy calls:", deploy.Calls)
}
