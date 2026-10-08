package modes

import (
	"context"
	"fmt"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/tui"
)

// startAttachedObserver keeps remote updates flowing while the editor is idle.
// Its cleanup joins the observer before Run restores the terminal or exits.
func (i *Interactive) startAttachedObserver(ctx context.Context) func() {
	if i.cfg.AttachedObserver == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := i.cfg.AttachedObserver(ctx, func(ev core.AgentEvent) {
			i.handleEvent(ev)
			i.invalidate()
		}, func() {
			i.mu.Lock()
			i.resetStreamingStateLocked()
			i.toolCalls = map[string]*tui.ToolCallView{}
			i.toolOrder = nil
			i.toolGate = map[string]int{}
			i.mu.Unlock()
			i.invalidate()
		})
		if err != nil && ctx.Err() == nil {
			i.mu.Lock()
			i.resetStreamingStateLocked()
			i.executionStatus = ""
			i.statusErr = fmt.Sprintf("attached updates stopped: %v, reconnect to the host", err)
			i.statusOK = ""
			i.mu.Unlock()
			i.invalidate()
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
