package swarm

import (
	"encoding/json"
	"strings"
)

type activeTool struct{ id, name string }

// toolActivity tracks the whole batch, including results arriving out of order.
func (s agentSink) toolActivity(ev Event) {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	id, _ := ev.Data["id"].(string)
	switch ev.Type {
	case "tool_call":
		name, _ := ev.Data["name"].(string)
		s.a.activeTools = append(s.a.activeTools, activeTool{id, name})
	case "tool_result":
		for n, tool := range s.a.activeTools {
			if tool.id == id {
				s.a.activeTools = append(s.a.activeTools[:n], s.a.activeTools[n+1:]...)
				break
			}
		}
	case "turn_end":
		s.a.activeTools = nil
		s.a.activity = "idle"
		return
	}
	s.a.activity = "thinking"
	if len(s.a.activeTools) > 0 {
		s.a.activity = "tool: " + truncate(s.a.activeTools[0].name, 60)
	}
}

func toolTranscript(ev Event) string {
	id, _ := ev.Data["id"].(string)
	if id == "" {
		return ""
	}
	switch ev.Type {
	case "tool_call":
		name, _ := ev.Data["name"].(string)
		header := "tool: " + name + " [" + id + "]"
		if args := ev.Data["args"]; args != nil {
			if raw, err := json.Marshal(args); err == nil {
				header += " " + string(raw)
			}
		}
		return header
	case "tool_result":
		header := "tool output: [" + id + "]"
		if failed, _ := ev.Data["is_error"].(bool); failed {
			header += " error"
		}
		lines := []string{header}
		// Events read from JSONL and host mirrors use the same decoded shape.
		if blocks, ok := ev.Data["content"].([]any); ok {
			for _, block := range blocks {
				m, _ := block.(map[string]any)
				if text, ok := m["text"].(string); ok {
					for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
						lines = append(lines, "tool output: "+line)
					}
				} else if kind, _ := m["type"].(string); kind != "" {
					lines = append(lines, "tool output: ["+kind+"]")
				}
			}
		}
		return strings.Join(lines, "\n")
	}
	return ""
}
