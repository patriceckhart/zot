package continuous

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/provider"
)

// slowClient streams text in several deltas with a pause so partial records
// are committed before the final message.
type slowClient struct {
	deltas []string
	pause  time.Duration
	fail   atomic.Bool
}

func (c *slowClient) Name() string { return "synthetic" }
func (c *slowClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 8)
	go func() {
		defer close(ch)
		var text strings.Builder
		for _, d := range c.deltas {
			select {
			case <-time.After(c.pause):
			case <-ctx.Done():
				ch <- provider.EventDone{Stop: provider.StopAborted, Err: ctx.Err()}
				return
			}
			text.WriteString(d)
			ch <- provider.EventTextDelta{Delta: d}
		}
		if c.fail.Load() {
			ch <- provider.EventDone{Stop: provider.StopError, Err: errors.New("HTTP 503 service unavailable")}
			return
		}
		ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: text.String()}}}}
	}()
	return ch, nil
}

// Streamed text is committed as partial records while the request runs, the
// final assistant commit removes the live record, and an interrupted attempt
// retains its text as a final partial that the next attempt replaces.
func TestPartialOutputIsCommittedAndCleared(t *testing.T) {
	ctx := context.Background()
	r, err := New(newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	// The pause is long relative to the flush interval so a partial record
	// is committed before the stream ends even on a loaded machine.
	client := &slowClient{deltas: []string{"hello ", "world"}, pause: 150 * time.Millisecond}
	svc, err := NewService(r, echoEngine(client, nil), ExecutionOptions{PartialFlushInterval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := r.OpenRoot(ctx, "w", AgentConfig{Model: "m"})
	r.Submit(ctx, c.ID, "a", "", "go")
	// Watch commits for partial records while the step runs.
	var mu sync.Mutex
	var texts []string
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	snap, _ := r.Snapshot(ctx)
	go r.Watch(watchCtx, c.ID, snap.Revision(), func(cm storage.Commit) error {
		if p, ok := PartialFromCommit(cm, c.ID); ok {
			mu.Lock()
			texts = append(texts, p.Text)
			mu.Unlock()
		}
		return nil
	})
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(texts) }
	run, _, err := svc.Step(ctx, c.ID)
	if err != nil || run.Outcome != "completed" {
		t.Fatalf("run: %+v %v", run, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for count() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	if len(texts) == 0 || !strings.HasPrefix(texts[0], "hello") {
		t.Fatalf("no partial records streamed: %v", texts)
	}
	mu.Unlock()
	if _, ok, _ := r.Partial(ctx, c.ID); ok {
		t.Fatal("live partial record survived the final commit")
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
	// A failing attempt retains its streamed text as final; the retry's
	// successful attempt clears it.
	client.fail.Store(true)
	svc2, _ := NewService(r, echoEngine(client, nil), ExecutionOptions{PartialFlushInterval: 5 * time.Millisecond, MaxAttempts: 2, RetryDelay: time.Millisecond})
	r.Submit(ctx, c.ID, "a", "", "again")
	interrupted := make(chan struct{})
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if p, ok, _ := r.Partial(ctx, c.ID); ok && p.Final {
				client.fail.Store(false)
				close(interrupted)
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	run, _, err = svc2.Step(ctx, c.ID)
	select {
	case <-interrupted:
	default:
		t.Fatal("failed attempt did not retain a final partial")
	}
	if err != nil || run.Outcome != "completed" || run.Attempt != 2 {
		t.Fatalf("retry run: %+v %v", run, err)
	}
	if _, ok, _ := r.Partial(ctx, c.ID); ok {
		t.Fatal("partial record not cleared after successful retry")
	}
	if report, err := r.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("integrity: %+v %v", report, err)
	}
}
