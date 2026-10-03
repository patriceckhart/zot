package core

import (
	"encoding/json"

	"github.com/patriceckhart/zot/packages/provider"
)

// withoutToolDisplay removes UI-only execution records from model inputs while
// preserving metadata needed for provider replay and tool-state restoration.
func withoutToolDisplay(messages []provider.Message) []provider.Message {
	out := append([]provider.Message(nil), messages...)
	for i, message := range out {
		if _, exists := message.Meta[provider.NestedToolCallsMetaKey]; !exists {
			continue
		}
		out[i].Meta = make(map[string]string, len(message.Meta)-1)
		for key, value := range message.Meta {
			if key != provider.NestedToolCallsMetaKey {
				out[i].Meta[key] = value
			}
		}
	}
	return out
}

func toolResultMetadata(state map[string]json.RawMessage, nested map[string][]provider.NestedToolCall) map[string]string {
	meta := toolStateMetadata(state)
	for id, calls := range nested {
		if len(calls) == 0 {
			delete(nested, id)
		}
	}
	if len(nested) > 0 {
		if raw, err := json.Marshal(nested); err == nil {
			if meta == nil {
				meta = map[string]string{}
			}
			meta[provider.NestedToolCallsMetaKey] = string(raw)
		}
	}
	return meta
}
