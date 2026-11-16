package core

import (
	"context"
	"errors"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestCompactWithStats(t *testing.T) {
	success := []provider.Event{provider.EventTextDelta{Delta: "summary"}, provider.EventDone{}}
	tool := []provider.Event{provider.EventDone{Stop: provider.StopToolUse}}
	failure := errors.New("summary failed")
	failed := []provider.Event{provider.EventDone{Err: failure}}
	for _, tc := range []struct {
		name      string
		mode      string
		responses [][]provider.Event
		fallback  bool
		err       error
	}{
		{name: "explicit side", mode: "side", responses: [][]provider.Event{success}},
		{name: "in place", responses: [][]provider.Event{success}},
		{name: "retry succeeds", responses: [][]provider.Event{tool, success}},
		{name: "tool fallback", responses: [][]provider.Event{tool, tool, success}, fallback: true},
		{name: "overflow fallback", responses: [][]provider.Event{{provider.EventDone{Err: errors.New("maximum context length exceeded")}}, success}, fallback: true},
		{name: "failed fallback", responses: [][]provider.Event{tool, tool, failed}, fallback: true, err: failure},
		{name: "terminal failure", responses: [][]provider.Event{failed}, err: failure},
		{name: "cancelled", responses: [][]provider.Event{{provider.EventDone{Err: context.Canceled}}}, err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &inPlaceClient{responses: tc.responses}
			a := NewAgent(client, "test", "", nil)
			a.CompactionMode = tc.mode
			if a.CompactionMode == "" {
				a.CompactionMode = "in_place"
			}
			a.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "work"}}}})
			summary, stats, err := a.CompactWithStats(context.Background(), 0, nil)
			if !errors.Is(err, tc.err) || stats.SideFallback != tc.fallback {
				t.Fatalf("stats = %+v, err = %v, want fallback %v, err %v", stats, err, tc.fallback, tc.err)
			}
			if len(client.requests) != len(tc.responses) {
				t.Fatalf("requests = %d, want %d", len(client.requests), len(tc.responses))
			}
			if err == nil && summary != "summary" {
				t.Fatalf("summary = %q", summary)
			}
		})
	}
}
