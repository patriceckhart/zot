package continuous

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

type legacyMessage struct {
	Role           provider.Role     `json:"role"`
	Content        []json.RawMessage `json:"content"`
	Time           json.RawMessage   `json:"time"`
	Meta           map[string]string `json:"meta"`
	AddedToolNames []string          `json:"added_tool_names"`
	calls          []string
	results        []string
}

// pairSessionMessages builds a projection without modifying original records.
// Only missing results are repaired. Orphan/duplicate results and malformed
// tool structures fail closed instead of being discarded or guessed successful.
func pairSessionMessages(raw []json.RawMessage) ([][]json.RawMessage, []SessionRepair, error) {
	messages := make([]legacyMessage, len(raw))
	for i, b := range raw {
		if err := json.Unmarshal(b, &messages[i]); err != nil {
			return nil, nil, fmt.Errorf("invalid message %d", i+1)
		}
		m := &messages[i]
		if m.Role != provider.RoleUser && m.Role != provider.RoleAssistant && m.Role != provider.RoleTool {
			return nil, nil, fmt.Errorf("invalid role at message %d", i+1)
		}
		if len(m.Time) > 0 {
			var timestamp time.Time
			if err := json.Unmarshal(m.Time, &timestamp); err != nil {
				return nil, nil, fmt.Errorf("invalid timestamp at message %d", i+1)
			}
		}
		seen := make(map[string]bool)
		for _, block := range m.Content {
			var obj map[string]json.RawMessage
			if err := json.Unmarshal(block, &obj); err != nil || obj == nil {
				return nil, nil, fmt.Errorf("invalid content at message %d", i+1)
			}
			var head struct {
				ID               string `json:"id"`
				Name             string `json:"name"`
				CallID           string `json:"call_id"`
				Server           bool   `json:"server"`
				Text             string `json:"text"`
				Format           string `json:"format"`
				MimeType         string `json:"mime_type"`
				Data             []byte `json:"data"`
				Summary          string `json:"summary"`
				ReasoningID      string `json:"reasoning_id"`
				Encrypted        string `json:"encrypted_content"`
				ThoughtSignature string `json:"thought_signature"`
			}
			if err := json.Unmarshal(block, &head); err != nil {
				return nil, nil, fmt.Errorf("invalid tool fields at message %d", i+1)
			}
			switch {
			case head.ID != "" || head.Name != "":
				if head.ID == "" || head.Name == "" || head.CallID != "" || m.Role != provider.RoleAssistant {
					return nil, nil, fmt.Errorf("invalid tool call at message %d", i+1)
				}
				if seen[head.ID] {
					return nil, nil, fmt.Errorf("duplicate tool call at message %d", i+1)
				}
				seen[head.ID] = true
				if !head.Server {
					m.calls = append(m.calls, head.ID)
				}
			case head.CallID != "":
				if m.Role != provider.RoleTool {
					return nil, nil, fmt.Errorf("tool result in wrong role at message %d", i+1)
				}
				var result struct {
					Content []json.RawMessage `json:"content"`
					IsError bool              `json:"is_error"`
				}
				if err := json.Unmarshal(block, &result); err != nil {
					return nil, nil, fmt.Errorf("invalid tool result at message %d", i+1)
				}
				for _, inner := range result.Content {
					var output *struct {
						Text     string `json:"text"`
						Format   string `json:"format"`
						MimeType string `json:"mime_type"`
						Data     []byte `json:"data"`
					}
					if err := json.Unmarshal(inner, &output); err != nil || output == nil {
						return nil, nil, fmt.Errorf("invalid tool output at message %d", i+1)
					}
				}
				m.results = append(m.results, head.CallID)
			default:
				if m.Role == provider.RoleTool {
					return nil, nil, fmt.Errorf("unidentified tool result at message %d", i+1)
				}
			}
		}
	}
	projection := make([][]json.RawMessage, len(raw))
	handledTool := make(map[int]bool)
	var notices []SessionRepair
	for i, m := range messages {
		// Legacy OpenSession omits empty-content rows from provider context.
		// Retain them in history without letting them break effective adjacency.
		if len(m.Content) == 0 {
			continue
		}
		if m.Role == provider.RoleTool && !handledTool[i] {
			return nil, nil, fmt.Errorf("orphan tool results at message %d", i+1)
		}
		if len(m.calls) == 0 {
			continue
		}
		pending := make(map[string]bool, len(m.calls))
		for _, id := range m.calls {
			pending[id] = true
		}
		next := i + 1
		for next < len(messages) && len(messages[next].Content) == 0 {
			next++
		}
		hasNext := next < len(messages) && messages[next].Role == provider.RoleTool
		if hasNext {
			for _, id := range messages[next].results {
				if !pending[id] {
					return nil, nil, fmt.Errorf("orphan or duplicate tool result at message %d", next+1)
				}
				delete(pending, id)
			}
			handledTool[next] = true
		}
		if len(pending) == 0 {
			continue
		}
		var missing []string
		var stubs []json.RawMessage
		for _, id := range m.calls {
			if !pending[id] {
				continue
			}
			missing = append(missing, id)
			stub := provider.ToolResultBlock{CallID: id, IsError: true, Content: []provider.Content{provider.TextBlock{Text: "tool call interrupted, no result recorded in imported session"}}}
			b, _ := json.Marshal(stub)
			stubs = append(stubs, b)
		}
		notices = append(notices, SessionRepair{Row: i, CallIDs: missing})
		if hasNext {
			content := append(append([]json.RawMessage(nil), messages[next].Content...), stubs...)
			b, _ := json.Marshal(content)
			projection[next] = []json.RawMessage{replaceJSONField(raw[next], "content", b)}
		} else {
			obj := map[string]any{"role": provider.RoleTool, "content": stubs}
			if len(m.Time) > 0 {
				obj["time"] = m.Time
			}
			b, _ := json.Marshal(obj)
			projection[i] = []json.RawMessage{raw[i], b}
		}
	}
	return projection, notices, nil
}
