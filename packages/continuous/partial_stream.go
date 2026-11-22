package continuous

import (
	"strings"

	"github.com/patriceckhart/zot/packages/core"
)

// partialStream projects checkpoints and RAM previews without duplicating overlapping
// reasoning suffixes. Attempt identity resets both streams, including retries
// whose answer text happens to start with the same prefix.
type partialStream struct {
	runID        string
	turn         int
	attempt      int
	started      bool
	text         string
	reasoningEnd int64
}

func (s *partialStream) reset() { *s = partialStream{} }

func (s *partialStream) apply(p Partial, sink func(core.AgentEvent)) {
	if p.Final || p.RunID == "" {
		return
	}
	// A durable checkpoint can lag the latest RAM preview. Never rewind
	// either stream when that older checkpoint arrives on the same watch.
	if s.runID == p.RunID && s.turn == p.Turn && s.attempt == p.Attempt &&
		len(p.Text) <= len(s.text) && strings.HasPrefix(s.text, p.Text) &&
		max(p.ReasoningBytes, int64(len(p.Reasoning))) <= s.reasoningEnd {
		return
	}
	if s.runID != p.RunID || s.turn != p.Turn || s.attempt != p.Attempt || !strings.HasPrefix(p.Text, s.text) {
		s.reset()
		s.runID, s.turn, s.attempt = p.RunID, p.Turn, p.Attempt
	}
	if !s.started && (p.Text != "" || p.Reasoning != "") {
		sink(core.EvAssistantStart{})
		s.started = true
	}
	end := p.ReasoningBytes
	if end < int64(len(p.Reasoning)) {
		end = int64(len(p.Reasoning))
	}
	if end > s.reasoningEnd {
		start := end - int64(len(p.Reasoning))
		offset := max(int64(0), s.reasoningEnd-start)
		if offset < int64(len(p.Reasoning)) {
			sink(core.EvReasoningDelta{Delta: p.Reasoning[offset:]})
		}
		s.reasoningEnd = end
	}
	if delta := strings.TrimPrefix(p.Text, s.text); delta != "" {
		sink(core.EvTextDelta{Delta: delta})
	}
	s.text = p.Text
}
