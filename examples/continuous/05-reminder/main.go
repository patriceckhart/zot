// A background reminder task whose timer survives a restart. The deadline is
// persisted; after the process comes back the overdue timer fires once.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/examples/continuous/internal/synthetic"
	"github.com/patriceckhart/zot/packages/continuous"
)

func registry(fired *int) continuous.TaskRegistry {
	reg := continuous.TaskRegistry{}
	synthetic.Must(reg.Register(continuous.TaskDefinition{
		Kind: "reminder", Version: 1,
		Initial: func(input json.RawMessage) (continuous.Next, error) {
			var in struct{ In string }
			json.Unmarshal(input, &in)
			d, err := time.ParseDuration(in.In)
			if err != nil {
				return continuous.Next{}, err
			}
			return continuous.Next{Phase: "fire", WakeAt: time.Now().Add(d)}, nil
		},
		Phases: map[string]func(context.Context, continuous.TaskContext) (continuous.Next, error){
			"fire": func(ctx context.Context, tc continuous.TaskContext) (continuous.Next, error) {
				*fired++
				return continuous.Next{Outcome: "completed", Result: "reminded"}, nil
			},
		},
	}))
	return reg
}

func main() {
	ctx := context.Background()
	store := synthetic.TempStore("reminder")
	fired := 0

	rt, err := synthetic.OpenStore(ctx, store)
	synthetic.Must(err)
	c, err := rt.OpenRoot(ctx, "reminders", continuous.AgentConfig{})
	synthetic.Must(err)
	task, err := rt.CreateTask(ctx, registry(&fired), c.ID, continuous.TaskSpec{Kind: "reminder", Input: map[string]string{"In": "300ms"}, Background: true})
	synthetic.Must(err)
	sched := continuous.NewTaskScheduler(rt, registry(&fired))
	// Run parks the task on its timer and would sleep until it fires; bound
	// it so we can "crash" instead.
	runCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	sched.Run(runCtx)
	cancel()
	parked, err := rt.Task(ctx, task.ID)
	synthetic.Must(err)
	fmt.Printf("timer persisted for %s, fired so far: %d\n", parked.WakeAt.Format(time.RFC3339Nano), fired)
	synthetic.Must(rt.Close())

	// Restart after the deadline passed while nobody was running.
	time.Sleep(400 * time.Millisecond)
	rt, err = synthetic.OpenStore(ctx, store)
	synthetic.Must(err)
	defer rt.Close()
	sched = continuous.NewTaskScheduler(rt, registry(&fired))
	synthetic.Must(sched.Run(ctx))
	final, err := rt.Task(ctx, task.ID)
	synthetic.Must(err)
	fmt.Printf("after restart: state=%s outcome=%s fired=%d\n", final.State, final.Outcome, fired)
}
