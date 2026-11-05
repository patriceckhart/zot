package core

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

type wipClient struct{ requests []provider.Request }

func (*wipClient) Name() string { return "test" }
func (c *wipClient) Stream(_ context.Context, req provider.Request) (<-chan provider.Event, error) {
	c.requests = append(c.requests, req)
	ch := make(chan provider.Event, 2)
	ch <- provider.EventTextDelta{Delta: "summary"}
	ch <- provider.EventDone{Stop: provider.StopEnd, Message: provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "summary"}}}}
	close(ch)
	return ch, nil
}

func TestWIPRequestsStayEphemeral(t *testing.T) {
	c := &wipClient{}
	a := NewAgent(c, "model", "stable system", nil)
	starts := 0
	a.BeforeStart = func(_ context.Context, s string) string {
		starts++
		return s
	}
	a.WIPBaseline = func() string { return strings.Repeat("a", 40) }
	for _, active := range []bool{false, true, true, false} {
		a.SetWIP(active)
		before := a.Messages()
		if err := a.Prompt(context.Background(), "clean text", nil, nil); err != nil {
			t.Fatal(err)
		}
		req := c.requests[len(c.requests)-1]
		if req.System != "stable system" || starts != 1 {
			t.Fatal("WIP changed system preparation")
		}
		count := strings.Count(serializeTranscript(req.Messages), "<wip-mode>")
		want := 0
		if active {
			want = 1
		}
		if count != want {
			t.Fatalf("blocks = %d, want %d", count, want)
		}
		for n := range before {
			if !reflect.DeepEqual(req.Messages[n], before[n]) {
				t.Fatalf("old turn %d changed", n)
			}
		}
		persisted := a.Messages()[len(before)]
		if persisted.WIP != active || MessageText(persisted) != "clean text" {
			t.Fatal("stored message was modified")
		}
	}
	if err := a.Continue(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(serializeTranscript(c.requests[len(c.requests)-1].Messages), "<wip-mode>") {
		t.Fatal("inactive continuation gained instructions")
	}
}

func TestWIPContinuationAndCompaction(t *testing.T) {
	c := &wipClient{}
	a := NewAgent(c, "model", "system", nil)
	a.WIPSettings.Instruction = "Custom draft instructions."
	a.SetWIP(true)
	if err := a.Prompt(context.Background(), "explore", nil, nil); err != nil {
		t.Fatal(err)
	}
	state := a.WIPState()
	a.SetWIP(true)
	if a.WIPState() != state {
		t.Fatal("idempotent enable recaptured baseline")
	}
	if err := a.Continue(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	wire := serializeTranscript(c.requests[1].Messages)
	if strings.Count(wire, "<wip-mode>") != 1 || !strings.Contains(wire, "Custom draft instructions.") || strings.Contains(wire, DefaultWIPInstruction) {
		t.Fatal(wire)
	}
	if _, err := a.Compact(context.Background(), 0, nil); err != nil {
		t.Fatal(err)
	}
	if !a.WIPState().Active {
		t.Fatal("compaction disabled WIP")
	}
	if err := a.Continue(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if strings.Count(serializeTranscript(c.requests[len(c.requests)-1].Messages), "<wip-mode>") != 1 {
		t.Fatal("compacted request lost instructions")
	}
}

func TestWIPSessionRoundTrip(t *testing.T) {
	s, err := NewSession(t.TempDir(), t.TempDir(), "test", "model", "test")
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
	a.SetWIP(true)
	if err := a.Prompt(context.Background(), "private clean text", nil, nil); err != nil {
		t.Fatal(err)
	}
	a.SetWIP(false)
	state := a.WIPState()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	meta, messages, err := ReadSession(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if meta.WIP == nil || *meta.WIP != state || !messages[0].WIP {
		t.Fatal("state lost")
	}
	if messages[0].Meta["wip_since"] != state.Since.Format(time.RFC3339Nano) {
		t.Fatal("draft phase identity lost")
	}
	data, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "<wip-mode>") {
		t.Fatal("ephemeral block persisted")
	}
	encoded, err := json.Marshal(messages[0])
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hydrateMessageObject(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(decoded)
	if err != nil || string(reencoded) != string(encoded) {
		t.Fatal("message metadata round trip failed")
	}
}

func TestWIPResetAndValidation(t *testing.T) {
	a := NewAgent(nil, "", "", nil)
	a.SetWIP(true)
	a.ResetWIP()
	if a.WIPState().Active {
		t.Fatal("reset ignored default off")
	}
	a.WIPSettings.DefaultOn = true
	a.ResetWIP()
	if !a.WIPState().Active {
		t.Fatal("reset ignored default on")
	}
	if err := (WIPSettings{Instruction: "</wip-mode>"}).Validate(); err == nil {
		t.Fatal("accepted wrapper")
	}
	if a.WIPDonePrompt() != DefaultWIPDonePrompt {
		t.Fatal("missing default cleanup")
	}
	a.WIPSettings.DonePrompt = "custom cleanup"
	if a.WIPDonePrompt() != "custom cleanup" {
		t.Fatal("ignored custom cleanup")
	}
}
