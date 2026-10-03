package core

import (
	"encoding/json"
	"strings"

	"github.com/patriceckhart/zot/packages/provider"
)

const toolStatePrefix = "tool_state:"

func cloneToolState(state map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(state))
	for key, value := range state {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

func toolStateMetadata(state map[string]json.RawMessage) map[string]string {
	if len(state) == 0 {
		return nil
	}
	meta := make(map[string]string, len(state))
	for key, value := range state {
		meta[toolStatePrefix+key] = string(value)
	}
	return meta
}

func readToolState(messages []provider.Message) map[string]json.RawMessage {
	state := map[string]json.RawMessage{}
	for _, message := range messages {
		for key, value := range message.Meta {
			if strings.HasPrefix(key, toolStatePrefix) && json.Valid([]byte(value)) {
				state[strings.TrimPrefix(key, toolStatePrefix)] = json.RawMessage(value)
			}
		}
	}
	return state
}
