package swarm

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage/memory"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// echoHostClient answers every request with the last user message.
type echoHostClient struct{}

func (echoHostClient) Name() string { return "synthetic" }
func (echoHostClient) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	ch := make(chan provider.Event, 3)
	text := "echo: " + core.MessageText(req.Messages[len(req.Messages)-1])
	ch <- provider.EventTextDelta{Delta: text}
	ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: text}}}}
	close(ch)
	return ch, nil
}

func newHostFixture(t *testing.T) (*continuous.HostServer, *continuous.Runtime, func(context.Context) (*continuous.Client, error)) {
	t.Helper()
	return newHostFixtureWithClient(t, echoHostClient{})
}

func newHostFixtureWithClient(t *testing.T, client provider.Client) (*continuous.HostServer, *continuous.Runtime, func(context.Context) (*continuous.Client, error)) {
	t.Helper()
	rt, err := continuous.New(memory.Open())
	if err != nil {
		t.Fatal(err)
	}
	engine := continuous.EngineFunc(func(ctx context.Context, c continuous.Conversation) (*core.Agent, error) {
		a := core.NewAgent(client, "echo", "", core.NewRegistry())
		a.MaxRetries = 0
		return a, nil
	})
	host, err := continuous.NewHost(rt, engine, continuous.HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go host.Run(ctx)
	t.Cleanup(func() { cancel(); rt.Close() })
	server := &continuous.HostServer{Host: host, Engine: engine, Version: "test"}
	dial := func(ctx context.Context) (*continuous.Client, error) {
		a, b := net.Pipe()
		go server.ServeConn(context.Background(), b)
		return continuous.NewClient(ctx, a, "")
	}
	return server, rt, dial
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// A swarm agent on a host runs as an owned conversation: the task is
// answered by the host, follow-up input is submitted (not sent over a
// socket), the transcript mirrors committed entries, and the event log
// carries the same lifecycle the dashboard replays. Resume attaches to the
// same conversation without re-running the task.
func TestHostRunnerRunsAgentAsOwnedConversation(t *testing.T) {
	_, rt, dial := newHostFixture(t)
	f := New(Config{Root: t.TempDir(), RepoRoot: t.TempDir(), NewRunner: NewHostRunnerFactory(dial, "workspace-a")})
	var turnEnds []int
	done := make(chan struct{}, 4)
	a, err := f.Spawn(context.Background(), "say hello")
	if err != nil {
		t.Fatal(err)
	}
	a.SetOnTurnEnd(func(step int, errMsg string) {
		turnEnds = append(turnEnds, step)
		done <- struct{}{}
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("task did not finish")
	}
	snap := a.Snapshot()
	if snap.Status != StatusRunning || snap.ConversationID == "" {
		t.Fatalf("snapshot: %+v", snap)
	}
	if !strings.Contains(strings.Join(snap.Lines, "\n"), "echo: say hello") {
		t.Fatalf("transcript missing host answer: %q", snap.Lines)
	}
	child, err := rt.Conversation(context.Background(), snap.ConversationID)
	if err != nil || child.Owner == nil || child.Owner.ID != "swarm/"+a.ID {
		t.Fatalf("child ownership: %+v %v", child, err)
	}
	root, err := rt.OpenRoot(context.Background(), "workspace-a", continuous.AgentConfig{})
	if err != nil || child.Owner.ConversationID != root.ID {
		t.Fatalf("child owned by root: %v %v", child.Owner, err)
	}

	// Follow-up input goes through the host as a new submission.
	if err := f.SendUserTurn(a.ID, "and again"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("follow-up did not finish")
	}
	waitUntil(t, "second answer", func() bool { return strings.Contains(strings.Join(a.Transcript(), "\n"), "echo: and again") })

	// Stop detaches; the conversation and its entries stay on the host.
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if a.Status() != StatusKilled {
		t.Fatalf("status after stop: %s", a.Status())
	}
	evs, err := ReadEventLog(a.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	joined := strings.Join(types, " ")
	for _, want := range []string{"agent_ready", "user_message", "assistant_message", "turn_end", "agent_stopped"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("event log missing %s: %v", want, types)
		}
	}

	// Resume attaches to the same owned conversation and does not resubmit
	// the task.
	child, err = rt.Conversation(context.Background(), snap.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	before := child.EntrySequence
	resumed, err := f.Resume(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "resumed conversation", func() bool { return resumed.Snapshot().ConversationID == snap.ConversationID })
	if err := f.SendUserTurn(a.ID, "third"); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "third answer", func() bool { return strings.Contains(strings.Join(resumed.Transcript(), "\n"), "echo: third") })
	after, err := rt.Conversation(context.Background(), snap.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	// One user entry and one assistant entry for "third": a resubmitted
	// task would add two more.
	if after.EntrySequence != before+2 {
		t.Fatalf("entries after resume: %d -> %d", before, after.EntrySequence)
	}
	if err := f.SendInput(a.ID, "shutdown"); err != nil {
		t.Fatal(err)
	}
	resumed.Wait()
	if resumed.Status() != StatusDone {
		t.Fatalf("status after shutdown: %s", resumed.Status())
	}
	if len(turnEnds) < 2 {
		t.Fatalf("turn ends: %v", turnEnds)
	}
}

// Sending to a hosted agent that is not running reports ErrNotReady, like a
// child whose inbox is closed.
func TestHostRunnerSendWhenStopped(t *testing.T) {
	_, _, dial := newHostFixture(t)
	f := New(Config{Root: t.TempDir(), RepoRoot: t.TempDir(), NewRunner: NewHostRunnerFactory(dial, "workspace-b")})
	a, err := f.Spawn(context.Background(), "task")
	if err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "conversation", func() bool { return a.Snapshot().ConversationID != "" })
	if err := f.Stop(a.ID); err != nil {
		t.Fatal(err)
	}
	a.Wait()
	if err := f.SendUserTurn(a.ID, "x"); err != ErrNotReady {
		t.Fatalf("send after stop: %v", err)
	}
}
