package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestSessionNameArguments(t *testing.T) {
	for _, flag := range []string{"-s", "--session-name"} {
		args, err := ParseArgs([]string{flag, "My_Session", "hello"})
		if err != nil || args.SessionName != "my_session" || args.Session != "" || args.Prompt != "hello" {
			t.Fatalf("parsed = %+v, %v", args, err)
		}
	}
	for _, raw := range [][]string{
		{"-s"}, {"-s", ""}, {"-s", "../escape"}, {"-s", "name", "--no-session"},
		{"-s", "name", "--continue"}, {"-s", "name", "--resume"}, {"-s", "name", "--session", "file"},
		{"-s", "name", "--continuous", "host.sock"},
		{"-s", "name", "--rpc"},
	} {
		if _, err := ParseArgs(raw); err == nil {
			t.Errorf("accepted %v", raw)
		}
	}
	args, err := ParseArgs([]string{"--session", "custom/path.jsonl"})
	if err != nil || args.Session != "custom/path.jsonl" || args.SessionName != "" {
		t.Fatal("changed --session PATH semantics")
	}
}

func TestNamedSessionStartupAndAgentNamespace(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	isolateSessionEnvironment(t)
	cwd := t.TempDir()
	r := Resolved{Provider: "test", Model: "model"}
	args := Args{CWD: cwd, SessionName: "investigation"}
	ag := core.NewAgent(nil, "model", "", nil)
	s, fresh, err := openOrCreateSessionState(args, r, ag, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !fresh || s.Meta.Alias != "investigation" || ag.SessionID != s.ID {
		t.Fatal("startup did not bind named session")
	}
	s.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "saved question"}}})
	if _, _, err := openOrCreateSessionState(args, r, ag, "test"); !errors.Is(err, core.ErrSessionInUse) {
		t.Fatalf("startup masked lock error: %v", err)
	}
	id := s.ID
	s.Close()
	ag = core.NewAgent(nil, "model", "", nil)
	s, fresh, err = openOrCreateSessionState(args, r, ag, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fresh || s.ID != id || len(ag.Messages()) != 1 || !strings.Contains(core.MessageText(ag.Messages()[0]), "saved question") {
		t.Fatal("named startup did not restore transcript")
	}
	args.AgentName = "other-agent"
	other, _, err := openOrCreateSessionState(args, r, core.NewAgent(nil, "model", "", nil), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.ID == id || other.Path == s.Path {
		t.Fatal("named agent namespace was ignored")
	}
}
