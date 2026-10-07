package continuous

import (
	"encoding/json"
	"testing"
)

func TestHostRootConfigurationDefaults(t *testing.T) {
	server, _, _, _ := newHostServer(t, nil)
	server.DefaultConfig = AgentConfig{Provider: "host-provider", Model: "host-model", Reasoning: "high"}
	client := dialHost(t, server)
	for _, tc := range []struct {
		workspace string
		config    *AgentConfig
		want      AgentConfig
	}{
		{"default", nil, server.DefaultConfig},
		{"partial", &AgentConfig{Model: "other-model", Tools: []string{"read"}}, AgentConfig{Provider: "host-provider", Model: "other-model", Reasoning: "high", Tools: []string{"read"}}},
		{"explicit", &AgentConfig{Provider: "other", Model: "other-model", Reasoning: "low"}, AgentConfig{Provider: "other", Model: "other-model", Reasoning: "low"}},
	} {
		data := client.must("conversation.create", map[string]any{"workspace": tc.workspace, "config": tc.config})
		var conv Conversation
		raw, _ := json.Marshal(data)
		if err := json.Unmarshal(raw, &conv); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(conv.Config)
		want, _ := json.Marshal(tc.want)
		if string(got) != string(want) {
			t.Fatalf("%s: config=%s want=%s", tc.workspace, got, want)
		}
		// Reopening a root must not replace its committed configuration.
		reopened := client.must("conversation.create", map[string]any{"workspace": tc.workspace, "config": AgentConfig{Model: "ignored"}})
		raw, _ = json.Marshal(reopened)
		if err := json.Unmarshal(raw, &conv); err != nil {
			t.Fatal(err)
		}
		got, _ = json.Marshal(conv.Config)
		if string(got) != string(want) {
			t.Fatalf("reopened %s changed config: %s", tc.workspace, got)
		}
	}
}
