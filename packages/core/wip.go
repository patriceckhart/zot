package core

import (
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

// WIPSettings controls draft instructions without changing the system prompt.
type WIPSettings struct {
	DefaultOn   bool   `json:"defaultOn,omitempty"`
	Instruction string `json:"instruction,omitempty"`
	DonePrompt  string `json:"donePrompt,omitempty"`
}

func (s WIPSettings) Validate() error {
	if strings.Contains(s.Instruction, "<wip-mode>") || strings.Contains(s.Instruction, "</wip-mode>") {
		return fmt.Errorf("wip.instruction must contain only the block body")
	}
	return nil
}

const DefaultWIPInstruction = `The user is iterating on an unfinished solution. Nothing written since WIP mode
started has been committed or deployed. Treat it as a draft:
- When an approach is replaced, remove the old one entirely, as if it never existed.
- No comments about what changed or why something was replaced.
- No fallbacks, compatibility shims, aliases, or deprecation paths for draft code.
- Remove unused code, imports, files, and tests left by abandoned attempts.
Code that existed before WIP mode started keeps normal care.`

const DefaultWIPDonePrompt = "WIP phase finished. Review every change made during this session's WIP phase. Remove leftovers of abandoned approaches, change-history comments, unused code, and unnecessary fallbacks. Do not change behaviour. Summarise what you removed."

// WIPState is per-session state. BaselineTurn counts accepted user turns.
type WIPState struct {
	Active         bool      `json:"active"`
	Since          time.Time `json:"since"`
	BaselineTurn   int       `json:"baselineTurn"`
	BaselineCommit string    `json:"baselineCommit,omitempty"`
}

type EvWIPChanged struct{ State WIPState }

func (EvWIPChanged) Type() string { return "wip_changed" }

func (a *Agent) WIPState() WIPState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.wip
}

// RestoreWIP restores persisted or inherited state without recapturing a baseline.
func (a *Agent) RestoreWIP(state WIPState) {
	a.mu.Lock()
	a.wip = state
	a.mu.Unlock()
}

func (a *Agent) SetWIP(active bool) {
	// Hosts capture git state, keeping repository operations out of core.
	commit := ""
	if active && !a.WIPState().Active && a.WIPBaseline != nil {
		commit = a.WIPBaseline()
	}
	a.mu.Lock()
	if a.wip.Active == active {
		a.mu.Unlock()
		return
	}
	a.wip.Active = active
	if active {
		turn := 0
		for _, m := range a.messages {
			if m.Role == provider.RoleUser && m.Meta["compaction"] != "true" {
				turn++
			}
		}
		a.wip = WIPState{Active: true, Since: time.Now().UTC(), BaselineTurn: turn, BaselineCommit: commit}
	}
	state := a.wip
	a.rev++
	a.mu.Unlock()
	a.publishWIP(state)
}

func (a *Agent) publishWIP(state WIPState) {
	if a.OnWIPChanged != nil {
		a.OnWIPChanged(state)
	}
	if a.OnEvent != nil {
		a.OnEvent(EvWIPChanged{State: state})
	}
}

func (a *Agent) ResetWIP() {
	state := WIPState{}
	if a.WIPSettings.DefaultOn {
		state.Active = true
		state.Since = time.Now().UTC()
		if a.WIPBaseline != nil {
			state.BaselineCommit = a.WIPBaseline()
		}
	}
	a.mu.Lock()
	a.wip = state
	a.rev++
	a.mu.Unlock()
	a.publishWIP(state)
}

func (a *Agent) WIPDonePrompt() string {
	if strings.TrimSpace(a.WIPSettings.DonePrompt) != "" {
		return a.WIPSettings.DonePrompt
	}
	return DefaultWIPDonePrompt
}

// markWIPLocked snapshots the instructions so exports do not depend on settings.
func (a *Agent) markWIPLocked(m *provider.Message) {
	if !a.wip.Active {
		return
	}
	m.WIP = true
	m.Meta = maps.Clone(m.Meta)
	if m.Meta == nil {
		m.Meta = make(map[string]string)
	}
	m.Meta["wip_since"] = a.wip.Since.Format(time.RFC3339Nano)
	m.WIPInstruction = a.WIPSettings.Instruction
	if m.WIPInstruction == "" {
		m.WIPInstruction = DefaultWIPInstruction
	}
	m.WIPBaselineCommit = a.wip.BaselineCommit
}

// withWIP only changes request copies. Older turns and persisted text stay clean.
func withWIP(messages []provider.Message) []provider.Message {
	for n := len(messages) - 1; n >= 0; n-- {
		m := messages[n]
		if m.Role != provider.RoleUser {
			continue
		}
		if !m.WIP {
			break
		}
		body := m.WIPInstruction
		if body == "" {
			body = DefaultWIPInstruction
		}
		if m.WIPBaselineCommit != "" {
			hash := m.WIPBaselineCommit
			if len(hash) > 12 {
				hash = hash[:12]
			}
			body += "\nWIP baseline: HEAD " + hash + ". Anything not in that commit is draft."
		}
		out := append([]provider.Message(nil), messages...)
		out[n].Content = append(append([]provider.Content(nil), m.Content...), provider.TextBlock{Text: "\n<wip-mode>\n" + body + "\n</wip-mode>"})
		return out
	}
	return messages
}
