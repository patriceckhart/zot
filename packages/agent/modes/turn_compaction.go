package modes

import (
	"context"
	"fmt"

	"github.com/patriceckhart/zot/packages/tui"
)

// compactBetweenTurns runs synchronously at the agent loop's safe boundary.
// The active run retains ownership of cancellation, queues, and busy state.
func (i *Interactive) compactBetweenTurns(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	i.mu.Lock()
	if !i.shouldAutoCompactLocked() {
		i.mu.Unlock()
		return nil
	}
	i.compacting = true
	i.autoCompacting = true
	i.resetStreamingStateLocked()
	i.spin.StartFixed("condensing history")
	i.mu.Unlock()
	i.invalidate()

	keepTail := 4
	if count := len(i.agent.Messages()); keepTail >= count {
		keepTail = count - 1
	}
	_, err := i.agent.Compact(ctx, keepTail, nil)

	i.mu.Lock()
	i.compacting = false
	i.autoCompacting = false
	var queued []string
	if err == nil {
		queued, i.queued = i.queued, nil
		i.lastCtxInput = estimateTimelineMessageTokens(i.agent.Messages())
		i.toolCalls = map[string]*tui.ToolCallView{}
		i.toolOrder = nil
		i.toolGate = map[string]int{}
		i.view.InvalidateRenderCache()
	}
	i.spin.Start()
	i.mu.Unlock()
	// Prompts entered during compaction were held by the host. Inject them
	// at this same safe boundary rather than waiting for the run to finish.
	for _, prompt := range queued {
		i.agent.QueueMessage(prompt)
	}
	i.invalidate()
	if err != nil {
		return fmt.Errorf("compaction failed: %w", err)
	}
	return nil
}
