package core

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestEmptyCompactionRecoveryBounded(t *testing.T) {
	empty := []provider.Event{provider.EventDone{}}
	tool := []provider.Event{provider.EventDone{Stop: provider.StopToolUse}}
	for _, tc := range []struct {
		name      string
		mode      string
		responses [][]provider.Event
		fallback  bool
	}{
		{name: "empty throughout", mode: "in_place", responses: [][]provider.Event{empty, empty, empty}, fallback: true},
		{name: "empty then tool", mode: "in_place", responses: [][]provider.Event{empty, tool, empty}, fallback: true},
		{name: "tool then empty", mode: "in_place", responses: [][]provider.Event{tool, empty, empty}, fallback: true},
		{name: "explicit side", mode: "side", responses: [][]provider.Event{empty}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &inPlaceClient{responses: tc.responses}
			a := NewAgent(client, "test", "system", nil)
			a.CompactionMode = tc.mode
			before := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work"}}}}
			a.SetMessages(before)
			persisted := false
			a.OnTranscriptCompacted = func([]provider.Message) { persisted = true }
			var events []EvCompact
			a.OnEvent = func(event AgentEvent) { events = append(events, event.(EvCompact)) }
			summary, stats, err := a.CompactWithStats(context.Background(), 0, nil)
			if err == nil || err.Error() != "empty summary from model" || summary != "" {
				t.Fatalf("summary=%q, err=%v", summary, err)
			}
			if len(client.requests) != len(tc.responses) || stats.SideFallback != tc.fallback {
				t.Fatalf("requests=%d, stats=%+v", len(client.requests), stats)
			}
			if persisted || !reflect.DeepEqual(a.Messages(), before) {
				t.Fatal("failed recovery changed or persisted transcript")
			}
			if len(events) != 2 || events[1].Status != "failed" || !errors.Is(events[1].Err, err) {
				t.Fatalf("unexpected lifecycle events: %+v", events)
			}
		})
	}
}

func TestEmptyCompactionRecoveryStopsOnErrors(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded, errors.New("authentication failed")} {
		t.Run(failure.Error(), func(t *testing.T) {
			client := &inPlaceClient{responses: [][]provider.Event{
				{provider.EventDone{}},
				{provider.EventDone{Err: failure}},
			}}
			a := NewAgent(client, "test", "system", nil)
			a.CompactionMode = "in_place"
			before := []provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work"}}}}
			a.SetMessages(before)
			_, stats, err := a.CompactWithStats(context.Background(), 0, nil)
			if !errors.Is(err, failure) || len(client.requests) != 2 || stats.SideFallback {
				t.Fatalf("err=%v, requests=%d, stats=%+v", err, len(client.requests), stats)
			}
			if !reflect.DeepEqual(a.Messages(), before) {
				t.Fatal("failed recovery changed transcript")
			}
		})
	}
}
