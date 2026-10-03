package core

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestToolStateCompactionAndPortableSessions(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	session, err := NewSession(root, cwd, "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	messages := []provider.Message{
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "start"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "first"}}, Meta: map[string]string{"tool_state:codemode": `{"value":1}`}},
		{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "next"}}},
		{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "second"}}, Meta: map[string]string{"tool_state:codemode": `{"value":2}`}},
	}
	for _, message := range messages {
		if err := session.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(serializeTranscript(messages), "value") {
		t.Fatal("private store entered model-facing text")
	}
	fork, err := BranchSession(session.Path, root, cwd, "test", 2)
	if err != nil {
		t.Fatal(err)
	}
	reopened, replayed, err := OpenSession(fork)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	if string(readToolState(replayed)["codemode"]) != `{"value":1}` {
		t.Fatalf("fork state: %+v", readToolState(replayed))
	}
	exported, err := ExportSession(session.Path, filepath.Join(t.TempDir(), "export.json"))
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ImportSession(exported, t.TempDir(), cwd, "test")
	if err != nil {
		t.Fatal(err)
	}
	reopened, replayed, err = OpenSession(imported)
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	if !reflect.DeepEqual(readToolState(replayed), readToolState(messages)) {
		t.Fatal("import lost store state")
	}
	ag := NewAgent(&compactLifecycleClient{}, "test", "", nil)
	ag.SetMessages(messages)
	if _, err := ag.Compact(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	compacted := ag.Messages()
	if string(readToolState(compacted)["codemode"]) != `{"value":2}` {
		t.Fatalf("compaction lost state: %+v", compacted)
	}
	resumed := NewAgent(nil, "test", "", nil)
	resumed.SetMessages(compacted)
	if string(resumed.toolState["codemode"]) != `{"value":2}` {
		t.Fatal("resume lost compacted state")
	}
	resumed.SetMessages(nil)
	if len(resumed.toolState) != 0 {
		t.Fatal("new session retained old store")
	}
}

func TestToolStateRejectsInvalidMetadataAndCopies(t *testing.T) {
	state := readToolState([]provider.Message{{Meta: map[string]string{"tool_state:codemode": `{"value":1}`, "tool_state:invalid": "not json"}}})
	if len(state) != 1 {
		t.Fatal(state)
	}
	cloned := cloneToolState(state)
	state["codemode"][0] = 'x'
	if !json.Valid(cloned["codemode"]) {
		t.Fatal("state snapshots share mutable bytes")
	}
}
