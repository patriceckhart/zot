package continuous

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

func TestClientWatchRejectsInvalidCursor(t *testing.T) {
	_, dial, hostCtx := attachHarness(t, &effectTool{name: "effect"}, nil)
	ctx, cancel := context.WithTimeout(hostCtx, 5*time.Second)
	defer cancel()
	c := dial()
	var conv Conversation
	if err := c.CallInto(ctx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	_, err := c.Watch(ctx, conv.ID, ^uint64(0))
	var ce *ClientError
	if !errors.As(err, &ce) || ce.Code != "cursor_expired" {
		t.Fatalf("invalid cursor: %v", err)
	}
	if calls, watches := c.pending(); calls != 0 || watches != 0 {
		t.Fatalf("pending calls=%d watches=%d", calls, watches)
	}
}

func TestClientWatchCancelsHostAfterLocalDrop(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	cancelled := make(chan struct{}, 1)
	go func() {
		reader := bufio.NewReader(b)
		enc := json.NewEncoder(b)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var req hostRequest
			if json.Unmarshal(line, &req) != nil {
				return
			}
			if req.Method == "watch.cancel" {
				cancelled <- struct{}{}
			}
			if err := enc.Encode(hostResponse{ID: req.ID, Type: "response", Success: true}); err != nil {
				return
			}
		}
	}()
	c, err := NewClient(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	watch, err := c.Watch(ctx, "w", 0)
	if err != nil {
		t.Fatal(err)
	}
	if dropWatches(c) != 1 {
		t.Fatal("watch not registered")
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal("locally dropped watch leaked on the host", ctx.Err())
	}
	select {
	case _, ok := <-watch:
		if ok {
			t.Fatal("unexpected commit")
		}
	case <-ctx.Done():
		t.Fatal("dropped watch did not close", ctx.Err())
	}
}

func TestClientWatchClosesOnTerminalError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() {
		reader := bufio.NewReader(b)
		enc := json.NewEncoder(b)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var req hostRequest
			if json.Unmarshal(line, &req) != nil {
				return
			}
			enc.Encode(hostResponse{ID: req.ID, Type: "response", Success: true})
			if req.Method == "conversation.watch" {
				enc.Encode(hostResponse{ID: req.ID, Type: "response", Success: false, Code: "storage", Error: "scan failed"})
			}
		}
	}()
	c, err := NewClient(ctx, a, "")
	if err != nil {
		t.Fatal(err)
	}
	watch, err := c.Watch(ctx, "w", 0)
	if err != nil {
		t.Fatal(err)
	}
	// A response on the same connection ensures the terminal error was read.
	if _, err := c.Call(ctx, "runtime.status", nil); err != nil {
		t.Fatal(err)
	}
	if _, watches := c.pending(); watches != 0 {
		t.Fatalf("terminal watch remains registered: %d", watches)
	}
	select {
	case _, ok := <-watch:
		if ok {
			t.Fatal("unexpected commit")
		}
	case <-ctx.Done():
		t.Fatal("watch did not close after terminal error")
	}
}
