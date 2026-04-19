package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// Tool implements core.Tool, exposing a `skill` tool the LLM can call
// to load the body of a discovered skill on demand. The system-prompt
// addendum lists the available names; this tool returns the full
// markdown for the requested one.
//
// The list of skills is held behind a mutex so tests / future
// /reload-skills wiring can swap in a fresh set without races.
type Tool struct {
	mu     sync.RWMutex
	skills []*Skill
}

// NewTool returns a skill loader tool seeded with the given skills.
// Pass the slice from Discover().
func NewTool(skills []*Skill) *Tool { return &Tool{skills: skills} }

// SetSkills atomically replaces the underlying skill set. Used when
// the user re-runs discovery (e.g. after editing a SKILL.md).
func (t *Tool) SetSkills(s []*Skill) {
	t.mu.Lock()
	t.skills = s
	t.mu.Unlock()
}

// Skills returns a snapshot for callers that need to render the
// current set (e.g. the /skills picker).
func (t *Tool) Skills() []*Skill {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]*Skill, len(t.skills))
	copy(out, t.skills)
	return out
}

// ---- core.Tool implementation ----

// Name is the LLM-facing tool name.
func (*Tool) Name() string { return "skill" }

// Description tells the model what this tool does. Kept blunt so the
// model reliably uses it instead of guessing what a "skill" is.
func (*Tool) Description() string {
	return "Load the full body of a named skill. Use this when the user's request matches one of the skills listed in the system prompt; the tool returns the skill's instructions, which you should then follow."
}

// Schema is one required string parameter: the skill name.
func (*Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {
				"type": "string",
				"description": "The skill name (must match one listed in the system prompt)."
			}
		},
		"required": ["name"]
	}`)
}

// Execute returns the markdown body of the requested skill.
func (t *Tool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return core.ToolResult{
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "skill: invalid args: " + err.Error()}},
		}, nil
	}
	if in.Name == "" {
		return core.ToolResult{
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: "skill: name is required"}},
		}, nil
	}

	t.mu.RLock()
	s := FindByName(t.skills, in.Name)
	t.mu.RUnlock()
	if s == nil {
		return core.ToolResult{
			IsError: true,
			Content: []provider.Content{provider.TextBlock{Text: fmt.Sprintf("skill: no skill named %q (run /skills in zot to see what's available)", in.Name)}},
		}, nil
	}

	header := fmt.Sprintf("# Skill: %s\n\n%s\n\n---\n\n", s.Name, s.Description)
	return core.ToolResult{
		Content: []provider.Content{provider.TextBlock{Text: header + s.Body}},
		Details: map[string]any{
			"skill": s.Name,
			"path":  s.Path,
		},
	}, nil
}
