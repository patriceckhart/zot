package core

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestWIPStateSurvivesWithoutPrompt(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	s, err := NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	a := NewAgent(nil, "", "", nil)
	a.SetWIP(true)
	if err := s.AppendWIP(a.WIPState()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	PruneEmptySessions(root, cwd)
	meta, _, err := ReadSession(s.Path)
	if err != nil || meta.WIP == nil || !meta.WIP.Active {
		t.Fatalf("lost WIP-only session: %v", err)
	}
}

func TestWIPPortableSessions(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "messages", true: "compacted"}[compact], func(t *testing.T) {
			root, cwd := t.TempDir(), t.TempDir()
			s, err := NewSession(root, cwd, "test", "model", "test")
			if err != nil {
				t.Fatal(err)
			}
			a := NewAgent(&wipClient{}, "model", "system", nil)
			a.OnWIPChanged = func(state WIPState) {
				if err := s.AppendWIP(state); err != nil {
					t.Fatal(err)
				}
			}
			a.OnMessageAppended = func(m provider.Message) {
				if err := s.AppendMessage(m); err != nil {
					t.Fatal(err)
				}
			}
			a.OnTranscriptCompacted = func(m []provider.Message) {
				if err := s.AppendCompaction(m); err != nil {
					t.Fatal(err)
				}
			}
			a.SetWIP(true)
			if err := a.Prompt(context.Background(), "draft", nil, nil); err != nil {
				t.Fatal(err)
			}
			if compact {
				if _, err := a.Compact(context.Background(), 0, nil); err != nil {
					t.Fatal(err)
				}
			}
			state := a.WIPState()
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			exported, err := ExportSession(s.Path, filepath.Join(t.TempDir(), "draft.zotsession"))
			if err != nil {
				t.Fatal(err)
			}
			imported, err := ImportSession(exported, t.TempDir(), cwd, "test")
			if err != nil {
				t.Fatal(err)
			}
			branch, err := BranchSession(s.Path, root, cwd, "test", len(a.Messages()))
			if err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{exported, imported, branch} {
				meta, messages, err := ReadSession(path)
				if err != nil {
					t.Fatal(err)
				}
				if meta.WIP == nil || *meta.WIP != state || len(messages) == 0 || !messages[0].WIP {
					t.Fatalf("lost WIP metadata in %s", path)
				}
			}
		})
	}
}
