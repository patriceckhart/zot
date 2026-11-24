package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/skills"
)

// userSkillSnapshot rediscovers user-facing skills with the same precedence
// as agent construction. Built-in implementation skills are not pinnable.
func userSkillSnapshot(args Args) []*skills.Skill {
	if args.NoSkill {
		return nil
	}
	userHome, _ := os.UserHomeDir()
	var sources []skills.Source
	if args.WithSkills {
		sources = append(sources, skills.SearchSources(ZotHome(), args.CWD, userHome)...)
	}
	if !args.NoExt || len(args.Exts) > 0 {
		extSources, _ := extensions.PlanSkillSources(ZotHome(), args.CWD, args.Exts, !args.NoExt)
		sources = append(extSources, sources...)
	}
	cfg, _ := LoadConfig()
	list, _ := skills.DiscoverSourcesExcluding(sources, true, resolveSkillExcludes(cfg.SkillExclude, args.CWD, userHome))
	return skills.VisibleSkills(list)
}

func preloadSkillPins(args Args, fresh bool, prompt string, diagnostics io.Writer) (string, string) {
	if !fresh || args.NoSkill {
		return prompt, ""
	}
	pins, err := loadSkillPins(args.CWD)
	if err != nil {
		fmt.Fprintln(diagnostics, "warning:", err)
		return prompt, ""
	}
	selected, warnings := pins.Resolve(userSkillSnapshot(args))
	for _, warning := range warnings {
		fmt.Fprintln(diagnostics, "warning:", warning)
	}
	return skills.PreloadPromptWithSnapshot(selected, prompt)
}

// Keep preferences local, even for project pins. Never write into a checkout.
type skillPinsFile struct {
	Global   []string            `json:"global,omitempty"`
	Projects map[string][]string `json:"projects,omitempty"`
}

var skillPinsMu sync.Mutex

func skillPinProject(cwd string) (string, error) {
	path, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	// Existing symlink aliases of a directory share their project pins.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path), nil
}

func readSkillPins() (skillPinsFile, error) {
	var pins skillPinsFile
	data, err := os.ReadFile(filepath.Join(ZotHome(), "skill-pins.json"))
	if errors.Is(err, os.ErrNotExist) {
		return pins, nil
	}
	if err != nil {
		return pins, fmt.Errorf("read skill pins: %w", err)
	}
	// Do not echo malformed file contents in diagnostics.
	if json.Unmarshal(data, &pins) != nil {
		return skillPinsFile{}, fmt.Errorf("invalid skill-pins.json")
	}
	return pins, nil
}

func loadSkillPins(cwd string) (skills.Pins, error) {
	skillPinsMu.Lock()
	defer skillPinsMu.Unlock()
	project, err := skillPinProject(cwd)
	if err != nil {
		return skills.Pins{}, err
	}
	pins, err := readSkillPins()
	return skills.Pins{Global: pins.Global, Project: pins.Projects[project]}, err
}

func toggleSkillPin(cwd, name string, global bool) error {
	if name == "" {
		return fmt.Errorf("skill name is required")
	}
	skillPinsMu.Lock()
	defer skillPinsMu.Unlock()
	project, err := skillPinProject(cwd)
	if err != nil {
		return err
	}
	pins, err := readSkillPins()
	if err != nil {
		return err
	}
	toggle := func(names []string) []string {
		if slices.Contains(names, name) {
			return slices.DeleteFunc(names, func(n string) bool { return n == name })
		}
		names = append(names, name)
		sort.Strings(names)
		return names
	}
	if global {
		pins.Global = toggle(pins.Global)
	} else {
		if pins.Projects == nil {
			pins.Projects = make(map[string][]string)
		}
		pins.Projects[project] = toggle(pins.Projects[project])
		if len(pins.Projects[project]) == 0 {
			delete(pins.Projects, project)
		}
	}
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(ZotHome(), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(ZotHome(), ".skill-pins-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(ZotHome(), "skill-pins.json")); err != nil {
		return fmt.Errorf("save skill pins: %w", err)
	}
	return nil
}
