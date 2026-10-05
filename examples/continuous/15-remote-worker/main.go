// Remote tool execution and reconciliation. A worker process runs the tool;
// the host loses the connection mid-call, records an unknown outcome, and
// after reconnecting reconciles the completed result without executing the
// effect a second time.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/patriceckhart/zot/examples/continuous/internal/synthetic"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type slowDeploy struct {
	started chan struct{}
	release chan struct{}
	calls   int
}

func (t *slowDeploy) Name() string            { return "deploy" }
func (t *slowDeploy) Description() string     { return "deploy on the worker" }
func (t *slowDeploy) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *slowDeploy) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	t.calls++
	t.started <- struct{}{}
	<-t.release
	return core.ToolResult{Content: []provider.Content{provider.TextBlock{Text: "deployed, operation " + core.ToolOperationKey(ctx)}}}, nil
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The worker: in production another process, here a goroutine on loopback.
	tool := &slowDeploy{started: make(chan struct{}, 1), release: make(chan struct{})}
	worker := &continuous.WorkerServer{Environment: "staging", Tools: core.NewRegistry(tool), Token: "worker-token-0123456789"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	synthetic.Must(err)
	go worker.Serve(ctx, ln)
	dial := func() *continuous.WorkerClient {
		conn, err := net.Dial("tcp", ln.Addr().String())
		synthetic.Must(err)
		c, err := continuous.DialWorker(ctx, conn, "worker-token-0123456789")
		synthetic.Must(err)
		return c
	}

	rt, err := synthetic.OpenStore(ctx, synthetic.TempStore("remote"))
	synthetic.Must(err)
	defer rt.Close()
	remote := &continuous.RemoteTool{Worker: dial(), ToolName: "deploy", Environment: "staging", Epoch: rt.Epoch}
	client := synthetic.NewClient(
		synthetic.Step{Calls: []provider.ToolCallBlock{synthetic.Call("c1", "deploy", `{"env":"staging"}`)}},
		synthetic.Step{Text: "Deployed."},
	)
	svc, err := continuous.NewService(rt, synthetic.Engine(client, remote), continuous.ExecutionOptions{})
	synthetic.Must(err)
	c, err := rt.OpenRoot(ctx, "ops", continuous.AgentConfig{Provider: "synthetic", Model: "synthetic-model"})
	synthetic.Must(err)
	_, err = rt.Submit(ctx, c.ID, "operator", "", "Deploy staging.")
	synthetic.Must(err)

	done := make(chan error, 1)
	go func() { _, _, err := svc.Step(ctx, c.ID); done <- err }()
	<-tool.started
	remote.Worker.Close() // connection lost while the worker is still deploying
	fmt.Println("step after disconnect:", <-done)
	plan, _ := rt.RecoveryPreview(ctx)
	fmt.Println("recovery:", plan.Actions[0].Action, "-", plan.Actions[0].Detail)

	close(tool.release) // the worker finishes on its own
	time.Sleep(50 * time.Millisecond)
	remote.Worker = dial()
	run, _, err := svc.Step(ctx, c.ID)
	synthetic.Must(err)
	fmt.Printf("run %s, worker executed the deploy %d time(s)\n", run.Outcome, tool.calls)
	for _, n := range run.Notices {
		fmt.Println("  notice:", n)
	}
}
