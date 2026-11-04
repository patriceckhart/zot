package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/provider"
)

func aliasTestMessage() provider.Message {
	return provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "synthetic question"}}}
}

func TestNormalizeSessionAlias(t *testing.T) {
	for _, input := range []string{"my_session", "Chatty-Lake", "a", strings.Repeat("a", 64)} {
		got, err := NormalizeSessionAlias(input)
		if err != nil || got != strings.ToLower(input) {
			t.Errorf("normalize %q = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "../escape", "/tmp/session", `a\b`, "-leading", "_leading", "two words", "a.jsonl", "a\n", "mäuse", strings.Repeat("a", 65)} {
		if _, err := NormalizeSessionAlias(input); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
}

func TestNamedSessionCreateResumeScopeAndLock(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	s, msgs, fresh, err := OpenNamedSession(root, cwd, "My_Session", "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !fresh || len(msgs) != 0 || s.Meta.Alias != "my_session" {
		t.Fatalf("created: fresh=%v, messages=%v, meta=%+v", fresh, msgs, s.Meta)
	}
	if _, err := uuid.Parse(s.ID); err != nil {
		t.Fatal("internal identity is not a UUID")
	}
	if err := s.AppendMessage(aliasTestMessage()); err != nil {
		t.Fatal(err)
	}
	for _, open := range []func() error{
		func() error {
			_, _, _, err := OpenNamedSession(root, cwd, "my_session", "test", "model", "test")
			return err
		},
		func() error { _, _, err := OpenSession(s.Path); return err },
		func() error { return DeleteSession(s.Path) },
	} {
		if err := open(); !errors.Is(err, ErrSessionInUse) {
			t.Fatalf("second writer/delete = %v", err)
		}
	}
	if _, got, err := ReadSession(s.Path); err != nil || len(got) != 1 {
		t.Fatalf("read while active = %v, %v", got, err)
	}
	id, path := s.ID, s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, msgs, fresh, err := OpenNamedSession(root, cwd, "MY_SESSION", "other", "other", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if fresh || reopened.ID != id || reopened.Path != path || len(msgs) != 1 {
		t.Fatal("resume changed identity or lost history")
	}
	if err := RenameSession(path, "Display title"); err != nil {
		t.Fatal(err)
	}
	if name, err := SessionName(path); err != nil || name != "Display title" {
		t.Fatalf("title = %q, %v", name, err)
	}
	if reopened.Meta.Alias != "my_session" {
		t.Fatal("rename changed lookup name")
	}
	other, _, fresh, err := OpenNamedSession(root, t.TempDir(), "my_session", "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if !fresh || other.ID == id {
		t.Fatal("alias leaked across working directories")
	}
}

func TestGeneratedSessionAliasesAndActivePruning(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	seen := map[string]bool{}
	for range 80 {
		s, err := NewSession(root, cwd, "test", "model", "test")
		if err != nil {
			t.Fatal(err)
		}
		if seen[s.Meta.Alias] || s.Meta.Alias == "" {
			t.Fatalf("duplicate or empty alias %q", s.Meta.Alias)
		}
		seen[s.Meta.Alias] = true
		if _, err := NormalizeSessionAlias(s.Meta.Alias); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendMessage(aliasTestMessage()); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	active, err := NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	PruneEmptySessions(root, cwd)
	if _, err := os.Stat(active.Path); err != nil {
		t.Fatalf("pruned active empty session: %v", err)
	}
	if name, err := SessionName(active.Path); err != nil || name != active.Meta.Alias {
		t.Fatalf("display name = %q, %v", name, err)
	}
	summaries := DescribeSessions(root, cwd)
	if len(summaries) != 81 {
		t.Fatalf("summaries = %d", len(summaries))
	}
	for _, summary := range summaries {
		if summary.Alias == "" {
			t.Fatal("summary lost alias")
		}
	}
	path := active.Path
	active.Close()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty session retained: %v", err)
	}
}

func TestNamedSessionConcurrentCreation(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	var wg sync.WaitGroup
	start := make(chan struct{})
	winners := make(chan *Session, 16)
	failures := make(chan error, 16)
	for range 16 {
		wg.Go(func() {
			<-start
			s, _, _, err := OpenNamedSession(root, cwd, "shared", "test", "test", "test")
			if err != nil {
				failures <- err
			} else {
				winners <- s
			}
		})
	}
	close(start)
	wg.Wait()
	close(winners)
	close(failures)
	count := 0
	for s := range winners {
		count++
		s.Close()
	}
	if count != 1 {
		t.Fatalf("writers = %d", count)
	}
	for err := range failures {
		if !errors.Is(err, ErrSessionInUse) {
			t.Errorf("loser = %v", err)
		}
	}
}

func TestPortableSessionsReceiveFreshAliases(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	parent, _, _, err := OpenNamedSession(root, cwd, "parent", "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	parent.AppendMessage(aliasTestMessage())
	branch, err := BranchSession(parent.Path, root, cwd, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	meta, _, err := ReadSession(branch)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Parent != parent.ID || meta.ID == parent.ID || meta.Alias == "" || meta.Alias == parent.Meta.Alias {
		t.Fatalf("branch meta = %+v", meta)
	}
	exported, err := ExportSession(parent.Path, filepath.Join(t.TempDir(), "portable.zotsession"))
	if err != nil {
		t.Fatal(err)
	}
	imported, err := ImportSession(exported, root, cwd, "test")
	if err != nil {
		t.Fatal(err)
	}
	meta, _, err = ReadSession(imported)
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID == parent.ID || meta.Alias == "" || meta.Alias == parent.Meta.Alias {
		t.Fatalf("import meta = %+v", meta)
	}
	resumed, _, fresh, err := OpenNamedSession(root, cwd, meta.Alias, "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if fresh || resumed.Path != imported {
		t.Fatal("imported alias does not resolve")
	}
}

func TestGeneratedAliasesHandleExhaustedWordPairs(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(SessionsDir(root, cwd), 0755); err != nil {
		t.Fatal(err)
	}
	// Fill every word pair to force the collision suffix, independent of RNG.
	for _, adjective := range sessionAdjectives {
		for _, noun := range sessionNouns {
			if err := os.WriteFile(aliasPath(root, cwd, adjective+"-"+noun), []byte("reserved"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	s, err := NewSession(root, cwd, "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(strings.Split(s.Meta.Alias, "-")) != 3 {
		t.Fatalf("expected collision suffix, got %q", s.Meta.Alias)
	}
	if _, err := NormalizeSessionAlias(s.Meta.Alias); err != nil {
		t.Fatal(err)
	}
}

func TestClosingOldSessionCannotDeleteReusedAlias(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	old, _, _, err := OpenNamedSession(root, cwd, "reused", "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	current, _, _, err := OpenNamedSession(root, cwd, "reused", "test", "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(current.Path); err != nil {
		t.Fatalf("stale Close removed current session: %v", err)
	}
	if _, _, err := OpenSession(current.Path); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("stale Close released current lease: %v", err)
	}
}
