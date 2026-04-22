package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/patriceckhart/zot/pkg/zotext"
)

type todoItem struct {
	Text string `json:"text"`
	Done bool   `json:"done"`
}

type store struct {
	Items  []todoItem `json:"items"`
	Cursor int        `json:"cursor"`
}

type toolArgs struct {
	Action   string `json:"action"`
	Title    string `json:"title"`
	NewTitle string `json:"new_title"`
}

type app struct {
	ext  *zotext.Extension
	mu   sync.Mutex
	path string
	st   store
	mode string
	edit string
}

const panelID = "todos-main"

func main() {
	ext := zotext.New("todo-panel", "0.3.0")
	a := &app{ext: ext}
	ext.Command("todo", "open a persistent todo panel", func(args string) zotext.Response {
		if err := a.ensureLoaded(); err != nil {
			return zotext.Errorf("load todos: %v", err)
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		return zotext.OpenPanel(panelID, a.title(), a.renderLines(), a.footer())
	})
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action":    map[string]any{"type": "string", "enum": []string{"list", "add", "complete", "edit", "remove"}},
			"title":     map[string]any{"type": "string"},
			"new_title": map[string]any{"type": "string"},
		},
		"required": []string{"action"},
	})
	ext.Tool("todo_manage", "List, add, complete, edit, or remove todos by title.", schema, a.handleTool)
	ext.OnPanelKey(panelID, a.handleKey, nil)
	if err := ext.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (a *app) ensureLoaded() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.path != "" {
		return nil
	}
	dir := a.ext.Host().DataDir
	if dir == "" {
		dir = a.ext.Host().ExtensionDir
	}
	if dir == "" {
		return fmt.Errorf("host did not provide extension_dir/data_dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	a.path = filepath.Join(dir, "todos.json")
	b, err := os.ReadFile(a.path)
	if err != nil {
		if os.IsNotExist(err) {
			a.st = store{Items: []todoItem{{Text: "Build something fun", Done: false}}}
			return a.saveLocked()
		}
		return err
	}
	if len(b) == 0 {
		a.st = store{}
		return nil
	}
	if err := json.Unmarshal(b, &a.st); err != nil {
		return err
	}
	a.clampLocked()
	return nil
}

func (a *app) saveLocked() error {
	a.clampLocked()
	b, err := json.MarshalIndent(a.st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.path, b, 0o644)
}

func (a *app) clampLocked() {
	if len(a.st.Items) == 0 {
		a.st.Cursor = 0
		return
	}
	if a.st.Cursor < 0 {
		a.st.Cursor = 0
	}
	if a.st.Cursor >= len(a.st.Items) {
		a.st.Cursor = len(a.st.Items) - 1
	}
}

func (a *app) title() string {
	done := 0
	for _, it := range a.st.Items {
		if it.Done {
			done++
		}
	}
	return fmt.Sprintf("Todos (%d/%d done)", done, len(a.st.Items))
}

func (a *app) renderLines() []string {
	if a.mode == "add" || a.mode == "edit" {
		label := "Add todo"
		if a.mode == "edit" {
			label = "Edit todo"
		}
		return []string{
			"  " + label,
			"",
			"  " + a.edit + "▌",
			"",
			"  Enter saves, Esc cancels",
		}
	}
	lines := []string{}
	if len(a.st.Items) == 0 {
		return []string{"  No todos yet.", "", "  Press a to add one."}
	}
	for i, it := range a.st.Items {
		cursor := "  "
		if i == a.st.Cursor {
			cursor = "› "
		}
		box := "□"
		if it.Done {
			box = "✓"
		}
		text := it.Text
		if it.Done {
			text += "  (done)"
		}
		lines = append(lines, cursor+box+" "+text)
	}
	return lines
}

func (a *app) footer() string {
	if a.mode == "add" || a.mode == "edit" {
		return "type text - enter save - esc cancel - backspace delete"
	}
	return "↑/↓ navigate - x toggle - a add - e edit - d delete - r refresh - esc close"
}

func (a *app) rerenderLocked() {
	a.ext.RenderPanel(panelID, a.title(), a.renderLines(), a.footer())
}

func (a *app) handleKey(key, text string) {
	if err := a.ensureLoaded(); err != nil {
		a.ext.Notify("error", fmt.Sprintf("todo: %v", err))
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.mode == "add" || a.mode == "edit" {
		a.handleEditModeLocked(key, text)
		return
	}
	switch key {
	case "up":
		a.st.Cursor--
	case "down":
		a.st.Cursor++
	case "rune":
		switch strings.ToLower(text) {
		case "x":
			if len(a.st.Items) > 0 {
				a.st.Items[a.st.Cursor].Done = !a.st.Items[a.st.Cursor].Done
			}
		case "d":
			if len(a.st.Items) > 0 {
				a.st.Items = append(a.st.Items[:a.st.Cursor], a.st.Items[a.st.Cursor+1:]...)
			}
		case "a":
			a.mode = "add"
			a.edit = ""
		case "e":
			if len(a.st.Items) > 0 {
				a.mode = "edit"
				a.edit = a.st.Items[a.st.Cursor].Text
			}
		case "r":
		}
	}
	a.clampLocked()
	if err := a.saveLocked(); err != nil {
		a.ext.Notify("error", fmt.Sprintf("save todos: %v", err))
	}
	a.rerenderLocked()
}

func (a *app) handleEditModeLocked(key, text string) {
	switch key {
	case "enter":
		trimmed := strings.TrimSpace(a.edit)
		if trimmed != "" {
			if a.mode == "add" {
				a.st.Items = append(a.st.Items, todoItem{Text: trimmed})
				a.st.Cursor = len(a.st.Items) - 1
			} else if a.mode == "edit" && len(a.st.Items) > 0 {
				a.st.Items[a.st.Cursor].Text = trimmed
			}
		}
		a.mode = ""
		a.edit = ""
	case "backspace":
		if len(a.edit) > 0 {
			r := []rune(a.edit)
			a.edit = string(r[:len(r)-1])
		}
	case "esc":
		a.mode = ""
		a.edit = ""
	case "rune":
		if text != "" {
			a.edit += text
		}
	}
	if err := a.saveLocked(); err != nil {
		a.ext.Notify("error", fmt.Sprintf("save todos: %v", err))
	}
	a.rerenderLocked()
}

func (a *app) handleTool(args json.RawMessage) zotext.ToolResult {
	if err := a.ensureLoaded(); err != nil {
		return zotext.TextErrorResult("load todos: " + err.Error())
	}
	var in toolArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return zotext.TextErrorResult("invalid args: " + err.Error())
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch in.Action {
	case "list":
		if len(a.st.Items) == 0 {
			return zotext.TextResult("No todos.")
		}
		var lines []string
		for _, it := range a.st.Items {
			mark := "[ ]"
			if it.Done {
				mark = "[x]"
			}
			lines = append(lines, mark+" "+it.Text)
		}
		return zotext.TextResult(strings.Join(lines, "\n"))
	case "add":
		if strings.TrimSpace(in.Title) == "" {
			return zotext.TextErrorResult("title is required for add")
		}
		a.st.Items = append(a.st.Items, todoItem{Text: strings.TrimSpace(in.Title)})
		a.st.Cursor = len(a.st.Items) - 1
	case "complete":
		idx := a.findByTitleLocked(in.Title)
		if idx < 0 {
			return zotext.TextErrorResult("todo not found: " + in.Title)
		}
		a.st.Items[idx].Done = true
		a.st.Cursor = idx
	case "edit":
		idx := a.findByTitleLocked(in.Title)
		if idx < 0 {
			return zotext.TextErrorResult("todo not found: " + in.Title)
		}
		if strings.TrimSpace(in.NewTitle) == "" {
			return zotext.TextErrorResult("new_title is required for edit")
		}
		a.st.Items[idx].Text = strings.TrimSpace(in.NewTitle)
		a.st.Cursor = idx
	case "remove":
		idx := a.findByTitleLocked(in.Title)
		if idx < 0 {
			return zotext.TextErrorResult("todo not found: " + in.Title)
		}
		a.st.Items = append(a.st.Items[:idx], a.st.Items[idx+1:]...)
	case "":
		return zotext.TextErrorResult("action is required")
	default:
		return zotext.TextErrorResult("unsupported action: " + in.Action)
	}
	if err := a.saveLocked(); err != nil {
		return zotext.TextErrorResult("save todos: " + err.Error())
	}
	if a.mode == "" {
		a.rerenderLocked()
	}
	return zotext.TextResult("ok")
}

func (a *app) findByTitleLocked(title string) int {
	needle := strings.TrimSpace(strings.ToLower(title))
	for i, it := range a.st.Items {
		if strings.TrimSpace(strings.ToLower(it.Text)) == needle {
			return i
		}
	}
	return -1
}
