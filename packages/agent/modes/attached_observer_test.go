package modes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestAttachedObserverInvalidatesIdleViewAndPreservesEditor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ag := core.NewAgent(nil, "test", "", nil)
	publish := make(chan struct{})
	published := make(chan struct{})
	stopped := make(chan struct{})
	iv := NewInteractive(InteractiveConfig{Agent: ag, AttachedObserver: func(ctx context.Context, sink func(core.AgentEvent), reset func()) error {
		select {
		case <-publish:
		case <-ctx.Done():
			return ctx.Err()
		}
		msg := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "remote prompt"}}}
		ag.SetMessages([]provider.Message{msg})
		reset()
		sink(core.EvUserMessage{Message: msg})
		close(published)
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}})
	iv.ed.SetValue("unsent local draft")
	iv.clipboardImages = []clipboardImageAttachment{{Marker: "[clipboard image #1]", Image: transferImage(t)}}
	stop := iv.startAttachedObserver(ctx)
	defer stop()
	for len(iv.dirty) > 0 {
		<-iv.dirty
	}
	close(publish)
	select {
	case <-published:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-iv.dirty:
	default:
		t.Fatal("idle remote update did not schedule a redraw")
	}
	if iv.busy || iv.ed.Value() != "unsent local draft" || len(iv.clipboardImages) != 1 {
		t.Fatal("remote update changed local input or started a local turn")
	}
	stop()
	select {
	case <-stopped:
	default:
		t.Fatal("observer cleanup did not join the subscription")
	}
}

func TestAttachedSubmissionDoesNotClearAnotherClientsStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	iv := NewInteractive(InteractiveConfig{
		Agent:            core.NewAgent(nil, "test", "", nil),
		AttachedObserver: func(context.Context, func(core.AgentEvent), func()) error { return nil },
		PromptDriverWithImages: func(ctx context.Context, _ *core.Agent, _ string, _ []provider.ImageBlock, _ func(core.AgentEvent)) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	iv.runCtx = ctx
	iv.handleEvent(core.EvAssistantStart{})
	iv.handleEvent(core.EvToolCall{ID: "remote-tool", Name: "read"})
	iv.mu.Lock()
	iv.streaming.WriteString("another client's partial answer")
	iv.mu.Unlock()
	iv.startTurn(ctx, "local follow-up")
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	iv.mu.Lock()
	preserved := iv.streamOn && iv.streaming.String() == "another client's partial answer" && iv.toolCalls["remote-tool"] != nil
	iv.mu.Unlock()
	if !preserved {
		t.Fatal("local submission erased observer-owned live output")
	}
	close(release)
	waitAttachedIdle(t, iv, false)
	iv.mu.Lock()
	defer iv.mu.Unlock()
	if !iv.streamOn {
		t.Fatal("local prompt completion hid the observer-owned stream")
	}
}

func TestAttachedObserverReportsConnectionFailure(t *testing.T) {
	ctx := context.Background()
	failed := make(chan struct{})
	iv := NewInteractive(InteractiveConfig{AttachedObserver: func(context.Context, func(core.AgentEvent), func()) error {
		<-failed
		return errors.New("connection closed")
	}})
	stop := iv.startAttachedObserver(ctx)
	close(failed)
	// Joining after the callback returns must not suppress a genuine error.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-iv.dirty:
			iv.mu.Lock()
			message := iv.statusErr
			iv.mu.Unlock()
			if strings.Contains(message, "attached updates stopped") && strings.Contains(message, "reconnect") {
				stop()
				return
			}
		case <-deadline:
			stop()
			t.Fatal("subscription failure was not surfaced")
		}
	}
}
