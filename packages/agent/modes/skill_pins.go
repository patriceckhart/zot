package modes

import (
	"slices"

	"github.com/patriceckhart/zot/packages/agent/skills"
	"github.com/patriceckhart/zot/packages/tui"
)

// SetPinnedSkillsPending arms preloading for a fresh conversation or disarms
// it after a session load. Changing models must not reset this state.
func (i *Interactive) SetPinnedSkillsPending(pending bool) {
	i.mu.Lock()
	i.pinnedSkillsPending = pending && i.cfg.LoadSkillPins != nil
	i.pinnedSkills = nil
	i.mu.Unlock()
	if pending {
		i.refreshSkillPins()
	}
	i.invalidate()
}

func (i *Interactive) refreshSkillPins() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.cfg.LoadSkillPins == nil {
		return
	}
	pins, err := i.cfg.LoadSkillPins(i.cfg.CWD)
	i.skillsDialog.pins = pins
	i.skillsDialog.canPin = i.cfg.ToggleSkillPin != nil
	var warnings []string
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	if i.pinnedSkillsPending {
		var available []*skills.Skill
		if i.cfg.SkillSnapshot != nil {
			available = i.cfg.SkillSnapshot()
		}
		var missing []string
		i.pinnedSkills, missing = pins.Resolve(available)
		warnings = append(warnings, missing...)
	}
	for _, warning := range warnings {
		if !slices.Contains(i.reloadErrors, warning) {
			i.reloadErrors = append(i.reloadErrors, warning)
		}
	}
}

func (i *Interactive) handleSkillPinKey(k tui.Key) bool {
	if k.Kind != tui.KeyRune || (k.Rune != 'p' && k.Rune != 'g') {
		return false
	}
	i.mu.Lock()
	d := i.skillsDialog
	if !d.Active() || d.viewing != nil || len(d.skills) == 0 || i.cfg.ToggleSkillPin == nil {
		i.mu.Unlock()
		return false
	}
	err := i.cfg.ToggleSkillPin(i.cfg.CWD, d.skills[d.cursor].Name, k.Rune == 'g')
	if err != nil {
		i.statusErr = "save skill pin: " + err.Error()
		i.statusOK = ""
	} else {
		i.statusErr = ""
		i.statusOK = "skill pins updated (apply on the next fresh conversation or /clear)"
	}
	i.mu.Unlock()
	if err == nil {
		i.refreshSkillPins()
	}
	return true
}

// Caller holds i.mu. Pending skills are folded into the real user message so
// normal event delivery and persistence include their bodies exactly once.
func (i *Interactive) preloadPinnedPromptLocked(prompt string) (string, string) {
	if !i.pinnedSkillsPending {
		return prompt, ""
	}
	i.pinnedSkillsPending = false
	selected := i.pinnedSkills
	i.pinnedSkills = nil
	return skills.PreloadPromptWithSnapshot(selected, prompt)
}

// Caller holds i.mu. Render a read-only notice, never editor placeholders.
func (i *Interactive) pinnedSkillsNotice(cols int) []string {
	if !i.pinnedSkillsPending || len(i.pinnedSkills) == 0 || cols < 4 {
		return nil
	}
	names := make([]string, 0, len(i.pinnedSkills))
	for _, s := range i.pinnedSkills {
		name := s.Name
		project := slices.Contains(i.skillsDialog.pins.Project, s.Name)
		global := slices.Contains(i.skillsDialog.pins.Global, s.Name)
		switch {
		case project && global:
			name += " (project + global)"
		case project:
			name += " (project)"
		case global:
			name += " (global)"
		}
		names = append(names, name)
	}
	return tui.RenderPinnedSkills(i.cfg.Theme, names, cols)
}
