package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/core"
)

func TestAutoSwarmSummaryIncludesCompleteFinalResponseAfterLongTask(t *testing.T) {
	response := "first response line\n" + strings.Repeat("result ", 150) + "final marker"
	mgr := swarm.New(swarm.Config{
		Root:     t.TempDir(),
		RepoRoot: t.TempDir(),
		NewRunner: func(*swarm.Agent) swarm.Runner {
			return swarm.RunnerFunc(func(_ context.Context, sink swarm.Sink) error {
				sink.Transcript(response)
				return nil
			})
		},
	})
	a, err := mgr.Spawn(context.Background(), strings.Repeat("long task ", 150))
	if err != nil {
		t.Fatal(err)
	}

	iv := newQueuedAutoSwarmInteractive()
	iv.TrackSwarmAgent(a, a.Task)
	update := waitForQueuedPrompt(t, iv)
	if !strings.Contains(update, "final response:\n"+response) {
		t.Fatalf("update missing complete final response: %q", update)
	}
}

func TestTrackSwarmAgentReportsStartupFailure(t *testing.T) {
	mgr := swarm.New(swarm.Config{
		Root:     t.TempDir(),
		RepoRoot: t.TempDir(),
		NewRunner: func(*swarm.Agent) swarm.Runner {
			return swarm.RunnerFunc(func(context.Context, swarm.Sink) error {
				return errors.New("listener startup failed")
			})
		},
	})
	a, err := mgr.Spawn(context.Background(), "report the date")
	if err != nil {
		t.Fatal(err)
	}

	iv := newQueuedAutoSwarmInteractive()
	iv.TrackSwarmAgent(a, a.Task)

	update := waitForQueuedPrompt(t, iv)
	if !strings.Contains(update, "status: failed") {
		t.Fatalf("update missing failed status: %q", update)
	}
	if !strings.Contains(update, "listener startup failed") {
		t.Fatalf("update missing startup error: %q", update)
	}
}

func TestCompleteSwarmWatchReportsTurnOutcomeOnce(t *testing.T) {
	started := make(chan struct{})
	mgr := swarm.New(swarm.Config{
		Root:     t.TempDir(),
		RepoRoot: t.TempDir(),
		NewRunner: func(*swarm.Agent) swarm.Runner {
			return swarm.RunnerFunc(func(ctx context.Context, _ swarm.Sink) error {
				close(started)
				<-ctx.Done()
				return ctx.Err()
			})
		},
	})
	a, err := mgr.Spawn(context.Background(), "report the date")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	t.Cleanup(func() {
		_ = mgr.Stop(a.ID)
		a.Wait()
	})

	iv := newQueuedAutoSwarmInteractive()
	entry := &swarmWatchEntry{agent: a, task: a.Task}
	iv.swarmWatch = []*swarmWatchEntry{entry}
	iv.completeSwarmWatchEntry(entry, "completed", "")
	iv.completeSwarmWatchEntry(entry, "failed", "late error")

	update := waitForQueuedPrompt(t, iv)
	if !strings.Contains(update, "status: completed") {
		t.Fatalf("update uses daemon status instead of turn outcome: %q", update)
	}
	iv.mu.Lock()
	queued := len(iv.queued)
	iv.mu.Unlock()
	if queued != 1 {
		t.Fatalf("queued updates = %d; want exactly one", queued)
	}
}

func newQueuedAutoSwarmInteractive() *Interactive {
	return &Interactive{
		agent:      &core.Agent{},
		busy:       true,
		compacting: true,
		dirty:      make(chan struct{}, 1),
	}
}

func waitForQueuedPrompt(t *testing.T, iv *Interactive) string {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		iv.mu.Lock()
		if len(iv.queued) > 0 {
			prompt := iv.queued[0].Text
			iv.mu.Unlock()
			return prompt
		}
		iv.mu.Unlock()
		select {
		case <-deadline.C:
			t.Fatal("timed out waiting for auto-swarm update")
		case <-ticker.C:
		}
	}
}
