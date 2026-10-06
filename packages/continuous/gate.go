package continuous

import (
	"sync"

	"github.com/patriceckhart/zot/packages/core"
)

// eventGate holds the engine events of one task invocation until a commit
// covers them, so no observer sees output that is not durable. Streamed text
// and tool progress are released after the partial or progress flush that
// recorded them; everything else of the phase after the phase's final
// commit. If that commit never happens (crash, stale invocation, abort),
// the held events are dropped, matching what a restarted observer reads.
type eventGate struct {
	sink func(core.AgentEvent)
	mu   sync.Mutex
	held []core.AgentEvent
	// base is the absolute sequence of held[0].
	base int
	// out serializes delivery so releases from a flush goroutine and the
	// final commit keep event order.
	out sync.Mutex
}

func newEventGate(sink func(core.AgentEvent)) *eventGate {
	return &eventGate{sink: sink}
}

// hold queues an event for a later release.
func (g *eventGate) hold(ev core.AgentEvent) {
	g.mu.Lock()
	g.held = append(g.held, ev)
	g.mu.Unlock()
}

// cover returns the absolute sequence just past the newest held event. A
// flush calls it before its commit and passes it to release after the
// commit succeeded; events held during the commit wait for the next one.
func (g *eventGate) cover() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.base + len(g.held)
}

// release publishes held events before the absolute sequence upto.
func (g *eventGate) release(upto int) {
	g.out.Lock()
	defer g.out.Unlock()
	g.mu.Lock()
	n := min(max(upto-g.base, 0), len(g.held))
	batch := append([]core.AgentEvent(nil), g.held[:n]...)
	g.held = append(g.held[:0], g.held[n:]...)
	g.base += n
	g.mu.Unlock()
	for _, ev := range batch {
		g.sink(ev)
	}
}

// releaseAll publishes every held event, then extra, after the phase's
// final commit.
func (g *eventGate) releaseAll(extra ...core.AgentEvent) {
	g.release(g.cover())
	g.out.Lock()
	defer g.out.Unlock()
	for _, ev := range extra {
		g.sink(ev)
	}
}
