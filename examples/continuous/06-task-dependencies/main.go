// A custom task with children, a fail_fast dependency wait, retries with a
// persisted backoff deadline, and compensation that runs before the task
// settles. Failing cleanup leaves the task blocked, never falsely terminal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/examples/continuous/internal/synthetic"
	"github.com/patriceckhart/zot/packages/continuous"
)

func main() {
	ctx := context.Background()
	rt, err := synthetic.OpenStore(ctx, synthetic.TempStore("tasks"))
	synthetic.Must(err)
	defer rt.Close()
	c, err := rt.OpenRoot(ctx, "deploy", continuous.AgentConfig{})
	synthetic.Must(err)

	attempts := 0
	refunds := 0
	reg := continuous.TaskRegistry{}
	synthetic.Must(reg.Register(continuous.TaskDefinition{
		Kind: "step", Version: 1,
		Initial: func(input json.RawMessage) (continuous.Next, error) { return continuous.Next{Phase: "run"}, nil },
		Phases: map[string]func(context.Context, continuous.TaskContext) (continuous.Next, error){
			"run": func(ctx context.Context, tc continuous.TaskContext) (continuous.Next, error) {
				var in struct{ Name string }
				json.Unmarshal(tc.Task.Input, &in)
				if in.Name == "flaky" {
					attempts++
					if attempts < 3 {
						return continuous.Next{}, fmt.Errorf("transient failure %d", attempts)
					}
				}
				if in.Name == "broken" {
					return continuous.Next{Outcome: "failed", Error: "deploy rejected"}, nil
				}
				return continuous.Next{Outcome: "completed", Result: in.Name + " done"}, nil
			},
		},
		Retry: continuous.RetryPolicy{MaxAttempts: 5, Backoff: 10 * time.Millisecond},
	}))
	synthetic.Must(reg.Register(continuous.TaskDefinition{
		Kind: "release", Version: 1,
		Initial: func(json.RawMessage) (continuous.Next, error) { return continuous.Next{Phase: "spawn"}, nil },
		Phases: map[string]func(context.Context, continuous.TaskContext) (continuous.Next, error){
			"spawn": func(ctx context.Context, tc continuous.TaskContext) (continuous.Next, error) {
				// Children commit with this phase; the wait is resolved on the
				// next pass against the committed children.
				return continuous.Next{Phase: "wait", Children: []continuous.TaskSpec{
					{Kind: "step", Input: map[string]string{"Name": "build"}},
					{Kind: "step", Input: map[string]string{"Name": "flaky"}},
					{Kind: "step", Input: map[string]string{"Name": "broken"}},
				}}, nil
			},
			"wait": func(ctx context.Context, tc continuous.TaskContext) (continuous.Next, error) {
				children, err := rt.Tasks(ctx, tc.Task.ConversationID)
				if err != nil {
					return continuous.Next{}, err
				}
				var ids []string
				for _, child := range children {
					if child.Owner == tc.Task.ID {
						ids = append(ids, child.ID)
					}
				}
				return continuous.Next{Phase: "finish", WaitOn: ids, WaitPolicy: "fail_fast"}, nil
			},
			"finish": func(ctx context.Context, tc continuous.TaskContext) (continuous.Next, error) {
				for _, w := range tc.Waited {
					if w.Outcome != "completed" {
						return continuous.Next{Outcome: "failed", Error: "a step failed: " + w.Error}, nil
					}
				}
				return continuous.Next{Outcome: "completed"}, nil
			},
		},
		Cleanup: func(ctx context.Context, tc continuous.TaskContext) error {
			refunds++
			if refunds == 1 {
				return errors.New("rollback endpoint unavailable")
			}
			return nil
		},
		Retry: continuous.RetryPolicy{MaxAttempts: 1},
	}))

	release, err := rt.CreateTask(ctx, reg, c.ID, continuous.TaskSpec{Kind: "release"})
	synthetic.Must(err)
	sched := continuous.NewTaskScheduler(rt, reg)
	synthetic.Must(sched.Run(ctx))

	tasks, _ := rt.Tasks(ctx, c.ID)
	for _, t := range tasks {
		fmt.Printf("%-8s state=%-9s outcome=%-9s attempts=%d blocked=%q\n", t.Kind, t.State, t.Outcome, t.Attempt, t.Blocked)
	}
	// The release failed, its cleanup failed once and exhausted its single
	// attempt, so it is blocked for a human. Retry after fixing the cause.
	_, err = rt.RetryCleanup(ctx, release.ID, "operator")
	synthetic.Must(err)
	synthetic.Must(sched.Run(ctx))
	final, _ := rt.Task(ctx, release.ID)
	fmt.Printf("release after cleanup retry: state=%s outcome=%s error=%q refunds=%d\n", final.State, final.Outcome, final.Error, refunds)
}
