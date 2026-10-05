// A read-only reviewer subagent: the main conversation delegates to a child
// conversation that runs with its own (read-only) tool set and configuration.
// Ownership is recorded, so aborting the parent run aborts the reviewer too.
package main

import (
	"context"
	"fmt"

	"github.com/patriceckhart/zot/examples/continuous/internal/synthetic"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func main() {
	ctx := context.Background()
	rt, err := synthetic.OpenStore(ctx, synthetic.TempStore("reviewer"))
	synthetic.Must(err)
	defer rt.Close()

	// The main agent calls the subagent tool; the child answers with the
	// read-only tool and a verdict. Scripted responses stand in for a model.
	client := synthetic.NewClient(
		synthetic.Step{Calls: []provider.ToolCallBlock{synthetic.Call("c1", "subagent", `{"task":"review the diff for risky writes","key":"review-1"}`)}},
		synthetic.Step{Calls: []provider.ToolCallBlock{synthetic.Call("r1", "read", `{"path":"diff.txt"}`)}},
		synthetic.Step{Text: "Review: the diff only touches tests. Approved."},
		synthetic.Step{Text: "The reviewer approved the change."},
	)
	reviewerRead := &synthetic.EchoTool{ToolName: "read", Replay: core.ReplaySafe}
	var svc *continuous.Service
	engine := continuous.EngineFunc(func(ctx context.Context, c continuous.Conversation) (*core.Agent, error) {
		tools := core.NewRegistry(reviewerRead)
		if c.Owner == nil {
			// The main conversation gets the subagent tool; the owned
			// reviewer conversation only gets read-only tools.
			tools["subagent"] = &continuous.SubagentTool{Runtime: rt, Service: svc, MaxDepth: 1}
		}
		a := core.NewAgent(client, "synthetic-model", "", tools)
		a.MaxRetries = 0
		return a, nil
	})
	svc, err = continuous.NewService(rt, engine, continuous.ExecutionOptions{})
	synthetic.Must(err)

	root, err := rt.OpenRoot(ctx, "reviewer-example", continuous.AgentConfig{Provider: "synthetic", Model: "synthetic-model"})
	synthetic.Must(err)
	_, err = rt.Submit(ctx, root.ID, "example", "", "Have the reviewer check the diff.")
	synthetic.Must(err)
	run, _, err := svc.Step(ctx, root.ID)
	synthetic.Must(err)
	fmt.Println("main run:", run.Outcome)

	children, err := rt.OwnedConversations(ctx, continuous.ToolCallIdentity{RunID: run.ID, ConversationID: root.ID, CallID: "c1"}.OwnerID())
	synthetic.Must(err)
	for _, id := range children {
		snap, err := rt.ConversationSnapshot(ctx, id, 20)
		synthetic.Must(err)
		fmt.Printf("reviewer conversation %s, owner %s, %d entries\n", id, snap.Conversation.Owner.ID, len(snap.Entries))
		for _, e := range snap.Entries {
			fmt.Printf("  %-12s %s\n", e.Type, e.Content)
		}
	}
	fmt.Println("reviewer read tool calls:", reviewerRead.Calls)
}
