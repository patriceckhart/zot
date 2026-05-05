package core

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/patriceckhart/zot/internal/provider"
)

// Compact summarizes the agent's transcript via the LLM and replaces
// it with a single synthetic user message carrying the summary. A
// small tail of recent messages is optionally preserved for continuity.
//
// keepTail is the number of most-recent messages to keep verbatim after
// the summary. 0 means summarize everything; a typical useful value is
// 4-8 (last couple of exchanges).
//
// The method blocks until the summary request completes. Emitted
// events via sink are limited to text deltas from the summary call so
// the UI can show progress.
func (a *Agent) Compact(ctx context.Context, keepTail int, sink func(delta string)) (summary string, err error) {
	a.mu.Lock()
	msgs := append([]provider.Message(nil), a.messages...)
	a.mu.Unlock()

	if len(msgs) == 0 {
		return "", fmt.Errorf("nothing to compact")
	}
	if keepTail < 0 {
		keepTail = 0
	}
	if keepTail > len(msgs) {
		keepTail = len(msgs)
	}
	summarizable := msgs[:len(msgs)-keepTail]
	if len(summarizable) == 0 {
		return "", fmt.Errorf("nothing to compact: keep-tail covers the whole transcript")
	}

	// Serialize the summarizable transcript to text and wrap it in tags
	// so the model treats it as material to summarize, not to continue.
	transcript := serializeTranscript(summarizable)

	prompt := "<conversation>\n" + transcript + "\n</conversation>\n\n" + compactionPrompt

	req := provider.Request{
		Model:     a.Model,
		System:    summarizationSystem,
		MaxTokens: 4096,
		Messages: []provider.Message{
			{
				Role:    provider.RoleUser,
				Content: []provider.Content{provider.TextBlock{Text: prompt}},
				Time:    time.Now(),
			},
		},
	}

	stream, err := a.Client.Stream(ctx, req)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	for ev := range stream {
		switch e := ev.(type) {
		case provider.EventTextDelta:
			sb.WriteString(e.Delta)
			if sink != nil {
				sink(e.Delta)
			}
		case provider.EventDone:
			if e.Err != nil {
				return "", e.Err
			}
		}
	}
	summary = strings.TrimSpace(sb.String())
	if summary == "" {
		return "", fmt.Errorf("empty summary from model")
	}

	// Estimate token count before compaction (rough: 1 token ~ 4 chars).
	tokensBefore := len(transcript) / 4

	// Replace transcript: one synthetic user message with the summary,
	// followed by the preserved tail (if any).
	synthetic := provider.Message{
		Role: provider.RoleUser,
		Content: []provider.Content{
			provider.TextBlock{Text: "## Context Summary (compacted)\n\n" + summary},
		},
		Time: time.Now(),
		Meta: map[string]string{
			"compaction":    "true",
			"tokens_before": strconv.Itoa(tokensBefore),
		},
	}

	tail := msgs[len(msgs)-keepTail:]
	// Repair the tail: remove orphaned tool_result blocks whose
	// matching tool_use was in the compacted (now-removed) portion.
	// Anthropic rejects transcripts where a tool_result references
	// a tool_use ID that doesn't exist.
	tail = repairOrphanedToolResults(tail)

	next := make([]provider.Message, 0, 1+len(tail))
	next = append(next, synthetic)
	next = append(next, tail...)

	a.mu.Lock()
	a.messages = next
	a.mu.Unlock()

	return summary, nil
}

// repairOrphanedToolResults removes tool_result content blocks (and
// entire messages that become empty) when the matching tool_use ID
// does not appear anywhere in the given messages. This happens after
// compaction when the tail preserves a tool_result but the tool_use
// that produced it was summarized away.
func repairOrphanedToolResults(msgs []provider.Message) []provider.Message {
	return provider.RepairOrphanedToolResults(msgs)
}

// serializeTranscript renders a list of provider.Message into a plain
// text transcript the summarization model can read without trying to
// continue the conversation.
func serializeTranscript(msgs []provider.Message) string {
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case provider.RoleUser:
			sb.WriteString("\n--- user ---\n")
		case provider.RoleAssistant:
			sb.WriteString("\n--- assistant ---\n")
		case provider.RoleTool:
			sb.WriteString("\n--- tool ---\n")
		}
		for _, c := range m.Content {
			switch v := c.(type) {
			case provider.TextBlock:
				sb.WriteString(v.Text)
				sb.WriteString("\n")
			case provider.ImageBlock:
				fmt.Fprintf(&sb, "[image: %s, %d bytes]\n", v.MimeType, len(v.Data))
			case provider.ToolCallBlock:
				fmt.Fprintf(&sb, "[tool_call %s %s]\n", v.Name, string(v.Arguments))
			case provider.ToolResultBlock:
				for _, inner := range v.Content {
					if tb, ok := inner.(provider.TextBlock); ok {
						sb.WriteString("[tool_result] ")
						sb.WriteString(tb.Text)
						sb.WriteString("\n")
					}
				}
			}
		}
	}
	return sb.String()
}

const summarizationSystem = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI coding assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

const compactionPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`
