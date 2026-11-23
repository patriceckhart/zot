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

func TestHistoricalWIPForks(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(map[bool]string{false: "nested", true: "compacted"}[compact], func(t *testing.T) {
			root, cwd := t.TempDir(), t.TempDir()
			s, err := NewSession(root, cwd, "test", "model", "test")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			a := NewAgent(nil, "", "", nil)
			appendMsg := func(text string) provider.Message {
				m := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: text}}}
				if err := s.AppendMessage(m); err != nil {
					t.Fatal(err)
				}
				return m
			}
			for range 20 {
				appendMsg("before")
			}
			a.SetWIP(true)
			state := a.WIPState()
			state.BaselineTurn = 20
			if err := s.AppendWIP(state); err != nil {
				t.Fatal(err)
			}
			draft := appendMsg("draft")
			off := state
			off.Active = false
			if err := s.AppendWIP(off); err != nil {
				t.Fatal(err)
			}
			next := appendMsg("after")
			cut, end := 21, 22
			if compact {
				summary := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "summary"}}, Meta: map[string]string{"compaction": "true"}}
				if err := s.AppendCompaction([]provider.Message{summary, draft, next}); err != nil {
					t.Fatal(err)
				}
				cut, end = 2, 3
			}
			fork := func(path string, n int) string {
				p, err := BranchSession(path, root, cwd, "test", n)
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			for _, path := range []string{s.Path, fork(s.Path, end)} {
				meta, _, err := ReadSession(fork(path, cut))
				if err != nil {
					t.Fatal(err)
				}
				if meta.WIP == nil || *meta.WIP != state {
					t.Fatalf("historical state = %+v, want %+v", meta.WIP, state)
				}
			}
		})
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
			activeState := a.WIPState()
			a.SetWIP(false)
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
			if !compact {
				historical, err := BranchSession(s.Path, root, cwd, "test", 1)
				if err != nil {
					t.Fatal(err)
				}
				meta, _, err := ReadSession(historical)
				if err != nil {
					t.Fatal(err)
				}
				if meta.WIP == nil || *meta.WIP != activeState {
					t.Fatalf("historical fork lost active WIP state: %+v", meta.WIP)
				}
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
