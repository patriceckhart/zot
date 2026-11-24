package skills

import (
	"fmt"
	"sort"
	"strings"
)

// Pins records independent global and project selections by discovered name.
type Pins struct {
	Global  []string `json:"global,omitempty"`
	Project []string `json:"project,omitempty"`
}

// Resolve returns each available pinned skill once, in name order.
func (p Pins) Resolve(available []*Skill) ([]*Skill, []string) {
	names := make(map[string]bool)
	for _, name := range append(append([]string(nil), p.Global...), p.Project...) {
		names[name] = true
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	var selected []*Skill
	var warnings []string
	for _, name := range sorted {
		if s := FindByName(available, name); s != nil {
			selected = append(selected, s)
		} else {
			warnings = append(warnings, fmt.Sprintf("pinned skill %q is unavailable", name))
		}
	}
	return selected, warnings
}

// PreloadPrompt includes pinned instructions with the first real request.
// It creates no synthetic tool calls and does not start a model turn.
func PreloadPrompt(selected []*Skill, request string) string {
	prompt, _ := PreloadPromptWithSnapshot(selected, request)
	return prompt
}

// PreloadPromptWithSnapshot returns the preloaded request and its exact pinned
// instruction prefix for transcript compaction.
func PreloadPromptWithSnapshot(selected []*Skill, request string) (prompt, instructions string) {
	if len(selected) == 0 {
		return request, ""
	}
	var b strings.Builder
	b.WriteString("Pinned skills for this conversation. Apply these instructions where relevant to the user's requests.\n\n")
	for _, s := range selected {
		b.WriteString(InvocationPrompt(s, ""))
		b.WriteString("\n\n")
	}
	instructions = strings.TrimSuffix(b.String(), "\n\n")
	return instructions + "\n\n---\n\nUser request:\n" + request, instructions
}
