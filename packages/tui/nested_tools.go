package tui

import "github.com/patriceckhart/zot/packages/provider"

func finalisedToolCalls(messages []provider.Message) map[string]bool {
	finalised := map[string]bool{}
	for _, message := range messages {
		nested := message.NestedToolCalls()
		for _, content := range message.Content {
			if result, ok := content.(provider.ToolResultBlock); ok {
				finalised[result.CallID] = true
				for _, call := range nested[result.CallID] {
					finalised[call.ID] = true
				}
			}
		}
	}
	return finalised
}

func (v *View) renderNestedToolCalls(calls []provider.NestedToolCall, width int) []string {
	if len(calls) == 0 {
		return nil
	}
	// Reuse completed-result rendering, including offsets, collapse modes and
	// error styling. Synthetic display blocks never enter the agent transcript.
	message := provider.Message{Role: provider.RoleTool}
	for _, call := range calls {
		message.Content = append(message.Content, provider.ToolResultBlock{
			CallID: call.ID, IsError: call.IsError,
			Content: []provider.Content{provider.TextBlock{Text: call.Result}},
		})
	}
	return v.renderMessage(message, width, false)
}
