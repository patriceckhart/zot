package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// A compaction entry replaces the model context before it with a summary and
// keeps the entries from Head onward verbatim. History is never deleted.
const entryCompaction = "compaction"

// CompactionPolicy controls automatic compaction inside a run. Zero disables.
type CompactionPolicy struct {
	// ContextWindow is the model's context size in tokens. Zero disables
	// automatic compaction.
	ContextWindow int
	// ReserveTokens is kept free for the response. Above
	// ContextWindow-ReserveTokens the next request compacts first.
	ReserveTokens int
	// KeepRecentTokens roughly bounds the verbatim tail kept after a summary.
	KeepRecentTokens int
	// BackgroundTokens starts summarization in the background once the
	// context exceeds it, while the run continues. The summary is published
	// at the next request boundary of that conversation if the context
	// boundary did not move. Zero disables background compaction; the
	// blocking threshold (ContextWindow-ReserveTokens) still applies.
	BackgroundTokens int
}

// CompactionInfo is the data of a compaction entry.
type CompactionInfo struct {
	// Head is the first entry sequence kept verbatim after the summary.
	Head uint64 `json:"head"`
	// Reason is manual, threshold, overflow, or background.
	Reason string `json:"reason"`
	// TokensBefore is a rough estimate of the summarized text.
	TokensBefore int `json:"tokens_before"`
	// ContextRevision is the conversation entry sequence the summary was
	// computed from. A later reset or compaction makes it stale.
	ContextRevision uint64 `json:"context_revision"`
}

// estimateTokens is the same rough heuristic the core engine uses.
func estimateTokens(messages []provider.Message) int {
	total := 0
	for _, m := range messages {
		for _, c := range m.Content {
			switch v := c.(type) {
			case provider.TextBlock:
				total += len(v.Text) / 4
			case provider.ToolCallBlock:
				total += (len(v.Name) + len(v.Arguments)) / 4
			case provider.ToolResultBlock:
				total += estimateTokens([]provider.Message{{Content: v.Content}})
			case provider.ImageBlock:
				total += 1000
			}
		}
	}
	return total
}

// selectCut chooses the first entry sequence to keep verbatim so the tail is
// about keepTokens. It never cuts between a tool call and its results, and
// never before an existing reset or compaction head. Zero means no cut.
func selectCut(ctx context.Context, snap storage.Snapshot, conversationID string, through uint64, keepTokens int) (uint64, error) {
	type item struct {
		seq    uint64
		tokens int
		safe   bool
	}
	var items []item
	err := History(ctx, snap, conversationID, through, func(owner string, seq uint64, e Entry) error {
		if owner != conversationID {
			// Inherited entries belong to a parent; a cut inside them would
			// orphan the fork's view. Cut only within the child's own entries.
			return nil
		}
		switch e.Type {
		case entryReset, entryCompaction:
			items = items[:0]
			return nil
		case entryAttempt:
			return nil
		}
		msg := provider.Message{}
		if len(e.Message) > 0 {
			var err error
			msg, err = core.DecodeMessage(e.Message)
			if err != nil {
				return fmt.Errorf("%w: %v", storage.ErrCorrupt, err)
			}
		} else if e.Type == "user" {
			msg = provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: e.Content}}}
		}
		// Safe boundaries: a user entry, or an assistant entry without tool calls.
		safe := e.Type == "user"
		if e.Type == entryAssistant {
			safe = true
			for _, c := range msg.Content {
				if _, ok := c.(provider.ToolCallBlock); ok {
					safe = false
				}
			}
		}
		items = append(items, item{seq: seq, tokens: estimateTokens([]provider.Message{msg}), safe: safe})
		return nil
	})
	if err != nil {
		return 0, err
	}
	// Walk backwards accumulating the tail until keepTokens is reached. The
	// candidate head is the entry where the accumulation crossed the budget;
	// move it forward to the next safe boundary so the tail starts at a
	// turn boundary and never inside a tool round. The first entry is never
	// a head (there would be nothing to summarize).
	acc := 0
	cut := -1
	for i := len(items) - 1; i >= 0; i-- {
		acc += items[i].tokens
		if acc > keepTokens {
			cut = i + 1
			break
		}
	}
	if cut <= 0 {
		// Everything fits in the budget: nothing to summarize.
		return 0, nil
	}
	for i := cut; i < len(items); i++ {
		if items[i].safe {
			return items[i].seq, nil
		}
	}
	// No safe boundary in the tail: summarize everything up to the last
	// entry only if that entry itself is safe, otherwise do not cut.
	return 0, nil
}

// Compact summarizes the context before head and commits a compaction entry.
// It is one model request between two commits: the compaction intent is the
// in-memory decision, the commit is the publication. A stale summary (the
// conversation changed its context boundary while summarizing) is discarded
// with an error rather than published. The conversation must be idle.
func (r *Runtime) Compact(ctx context.Context, engine Engine, conversationID, instructions string, keepTokens int) (Conversation, error) {
	return r.compact(ctx, engine, conversationID, instructions, keepTokens, "manual", nil)
}

// ErrCompactionStale reports a summary whose source range was superseded by a
// reset or another compaction before publication. The summary is discarded.
var ErrCompactionStale = errors.New("compaction stale: the context boundary moved while summarizing")

// errNothingToCompact reports that the context fits within the keep budget.
var errNothingToCompact = errors.New("nothing to compact")

// pendingCompaction is a summary computed over a fixed source range that has
// not been published yet.
type pendingCompaction struct {
	conversationID string
	// head is the first entry kept verbatim; the summary covers everything
	// before it in the context that was current at contextRevision.
	head            uint64
	contextRevision uint64
	reason          string
	summary         string
	tokensBefore    int
	provider, model string
	usage           provider.Usage
	usageKnown      bool
}

func (r *Runtime) compact(ctx context.Context, engine Engine, conversationID, instructions string, keepTokens int, reason string, sink func(core.AgentEvent)) (Conversation, error) {
	pending, err := r.summarize(ctx, engine, conversationID, instructions, keepTokens, reason, sink)
	if err != nil {
		if errors.Is(err, errNothingToCompact) {
			c, cErr := r.Conversation(ctx, conversationID)
			if cErr != nil {
				return Conversation{}, cErr
			}
			return c, err
		}
		return Conversation{}, err
	}
	return r.publishCompaction(ctx, pending, sink)
}

// summarize selects the cut and runs the summary request. It performs no
// commit: the result is published separately so the request can overlap with
// execution.
func (r *Runtime) summarize(ctx context.Context, engine Engine, conversationID, instructions string, keepTokens int, reason string, sink func(core.AgentEvent)) (*pendingCompaction, error) {
	if keepTokens <= 0 {
		keepTokens = 20000
	}
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	c, err := conversation(snap, conversationID)
	if err != nil {
		return nil, err
	}
	if run, ok, err := read[Run](snap, runKey(conversationID)); err != nil {
		return nil, err
	} else if ok && run.Phase != "done" && reason == "manual" {
		return nil, ErrBusy
	}
	head, err := selectCut(ctx, snap, conversationID, c.EntrySequence, keepTokens)
	if err != nil {
		return nil, err
	}
	if head == 0 {
		return nil, errNothingToCompact
	}
	// Messages before the head, within the current context.
	before, err := ModelContext(ctx, snap, conversationID, head-1)
	if err != nil {
		return nil, err
	}
	if len(before) == 0 {
		return nil, errNothingToCompact
	}
	agent, err := engine.Build(ctx, c)
	if err != nil {
		return nil, err
	}
	if c.Config.Model != "" {
		agent.Model = c.Config.Model
	}
	agent.SessionID = c.ID
	pending := &pendingCompaction{conversationID: conversationID, head: head, contextRevision: c.EntrySequence, reason: reason, provider: c.Config.Provider, model: agent.Model}
	baseline := agent.Cost()
	agent.OnUsage = func(cum provider.Usage) {
		// OnUsage reports the cumulative total; the summary's own usage is
		// the increase over the agent's total before the request.
		pending.usage = provider.Usage{InputTokens: cum.InputTokens - baseline.InputTokens, OutputTokens: cum.OutputTokens - baseline.OutputTokens, ReasoningTokens: cum.ReasoningTokens - baseline.ReasoningTokens, CacheReadTokens: cum.CacheReadTokens - baseline.CacheReadTokens, CacheWriteTokens: cum.CacheWriteTokens - baseline.CacheWriteTokens, CostUSD: cum.CostUSD - baseline.CostUSD}
		pending.usageKnown = true
	}
	if sink != nil {
		sink(core.EvCompact{Phase: "pre", ID: c.ID, MessageCount: len(before), TokenEstimate: estimateTokens(before)})
	}
	pending.summary, pending.tokensBefore, err = agent.Summarize(ctx, before, instructions, nil)
	if err != nil {
		if sink != nil {
			sink(core.EvCompact{Phase: "post", ID: c.ID, Status: "failed", Err: err})
		}
		return nil, err
	}
	return pending, nil
}

// publishCompaction commits a summary as a compaction entry after checking
// that its source range is still the active context. New entries after the
// head are kept verbatim; a reset or compaction at or after the head makes
// the summary stale and it is discarded.
func (r *Runtime) publishCompaction(ctx context.Context, p *pendingCompaction, sink func(core.AgentEvent)) (Conversation, error) {
	conversationID, head := p.conversationID, p.head
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Conversation{}, err
		}
		latest, err := conversation(snap, conversationID)
		if err != nil {
			return Conversation{}, err
		}
		// Stale check: the context boundary must not have moved past the
		// range we summarized. New entries after the head are fine (they are
		// kept verbatim); a reset or compaction at or after the head is not.
		stale := false
		if err := History(ctx, snap, conversationID, latest.EntrySequence, func(owner string, seq uint64, e Entry) error {
			if owner == conversationID && seq >= head && (e.Type == entryReset || e.Type == entryCompaction) {
				stale = true
			}
			return nil
		}); err != nil {
			return Conversation{}, err
		}
		if stale {
			if sink != nil {
				sink(core.EvCompact{Phase: "post", ID: conversationID, Status: "failed", Err: ErrCompactionStale})
			}
			return latest, ErrCompactionStale
		}
		latest.EntrySequence++
		latest.UsageSequence++
		latest.Revision = snap.Revision() + 1
		info := CompactionInfo{Head: head, Reason: p.reason, TokensBefore: p.tokensBefore, ContextRevision: p.contextRevision}
		entry := Entry{ID: uuid.NewString(), ConversationID: conversationID, Revision: latest.Revision, Type: entryCompaction, Content: p.summary, Time: time.Now().UTC()}
		entry.Message = marshalMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "## Context Summary (compacted)\n\n" + p.summary}}, Time: time.Now().UTC(), Meta: map[string]string{"compaction": "true", "head": fmt.Sprint(head)}})
		entry.Data = marshalAny(info)
		status := "unknown"
		if p.usageKnown {
			status = "known"
		}
		ledger := record(usageKey(conversationID, latest.UsageSequence), UsageRecord{ConversationID: conversationID, RunID: "compaction", Turn: 1, Attempt: 1, Provider: p.provider, Model: p.model, Usage: p.usage, Status: status, Source: "model", Tool: "compaction", Time: time.Now().UTC()})
		err = r.commit(ctx, snap, "conversation.compact", record("conversation/"+conversationID, latest), record(entryKey(conversationID, latest.EntrySequence), entry), ledger)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		if sink != nil {
			sink(core.EvCompact{Phase: "post", ID: conversationID, Status: "completed", TokenEstimate: p.tokensBefore})
		}
		return latest, err
	}
}

func marshalAny(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// needsCompaction reports whether the estimated context exceeds the policy
// threshold.
func needsCompaction(policy CompactionPolicy, messages []provider.Message, system string) bool {
	if policy.ContextWindow <= 0 {
		return false
	}
	limit := policy.ContextWindow - policy.ReserveTokens
	if limit <= 0 {
		return false
	}
	return estimateTokens(messages)+len(system)/4 > limit
}

// isContextOverflow recognises provider rejections of an oversized request.
func isContextOverflow(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{"context length", "context_length", "maximum context", "too many tokens", "prompt is too long", "request too large", "context window", "exceeds the model", "input is too long", "token limit"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// backgroundCompactor runs at most one summary per conversation concurrently
// with execution and holds the result until a request boundary publishes it.
type backgroundCompactor struct {
	mu      sync.Mutex
	running map[string]bool
	ready   map[string]*pendingCompaction
	// stale counts summaries discarded at publication, for metrics.
	stale    uint64
	started  uint64
	finished uint64
	wg       sync.WaitGroup
}

func newBackgroundCompactor() *backgroundCompactor {
	return &backgroundCompactor{running: map[string]bool{}, ready: map[string]*pendingCompaction{}}
}

// start begins a summary for the conversation unless one is running or
// waiting for publication. The summary runs on ctx, which is the host's
// context, so shutdown cancels it; a cancelled summary is never published.
func (b *backgroundCompactor) start(ctx context.Context, r *Runtime, engine Engine, conversationID string, keepTokens int, sink func(core.AgentEvent)) bool {
	b.mu.Lock()
	if b.running[conversationID] || b.ready[conversationID] != nil {
		b.mu.Unlock()
		return false
	}
	b.running[conversationID] = true
	b.started++
	b.mu.Unlock()
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		pending, err := r.summarize(ctx, engine, conversationID, "", keepTokens, "background", sink)
		b.mu.Lock()
		delete(b.running, conversationID)
		b.finished++
		if err == nil {
			b.ready[conversationID] = pending
		}
		b.mu.Unlock()
	}()
	return true
}

// take removes and returns a summary waiting for publication.
func (b *backgroundCompactor) take(conversationID string) *pendingCompaction {
	b.mu.Lock()
	defer b.mu.Unlock()
	p := b.ready[conversationID]
	delete(b.ready, conversationID)
	return p
}

func (b *backgroundCompactor) markStale() {
	b.mu.Lock()
	b.stale++
	b.mu.Unlock()
}

// CompactionMetrics reports background compaction activity.
type CompactionMetrics struct {
	Started  uint64 `json:"started"`
	Finished uint64 `json:"finished"`
	Running  int    `json:"running"`
	Pending  int    `json:"pending"`
	Stale    uint64 `json:"stale"`
}

func (b *backgroundCompactor) metrics() CompactionMetrics {
	b.mu.Lock()
	defer b.mu.Unlock()
	return CompactionMetrics{Started: b.started, Finished: b.finished, Running: len(b.running), Pending: len(b.ready), Stale: b.stale}
}

// wait blocks until running summaries finish or ctx ends.
func (b *backgroundCompactor) wait(ctx context.Context) {
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
