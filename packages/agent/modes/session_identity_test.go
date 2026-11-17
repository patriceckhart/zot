package modes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestSessionIdentityViewsFollowCurrentSession(t *testing.T) {
	current := sessionIdentity{ID: "11111111-1111-4111-8111-111111111111", Alias: "tidy-birch"}
	i := &Interactive{
		cfg: InteractiveConfig{
			CurrentSessionIdentity: func() (string, string) { return current.ID, current.Alias },
			// Explicit session filenames need not contain the session UUID.
			CurrentSessionPath: func() string { return "/synthetic/custom-name.jsonl" },
		},
		timeline:         newTimelineView(),
		sessionOpsDialog: newSessionOpsDialog(),
	}
	i.openSessionOpsDialog()
	i.openTimeline()
	for _, next := range []sessionIdentity{
		current,
		{ID: "22222222-2222-4222-8222-222222222222", Alias: "quiet-lake"},
		{ID: "33333333-3333-4333-8333-333333333333"},
		{},
	} {
		previous := current
		current = next
		data := i.timelineData()
		if data.Session != current {
			t.Fatalf("timeline session=%+v, want %+v", data.Session, current)
		}
		views := [][]string{
			i.timeline.Render(tui.Theme{}, 100, 40, data),
			i.sessionOpsDialog.Render(tui.Theme{}, 100, i.currentSessionIdentity()),
		}
		for _, rows := range views {
			text := strings.Join(rows, "\n")
			if current.ID == "" {
				if !strings.Contains(text, "no active session") {
					t.Fatalf("missing empty state: %s", text)
				}
			} else if !strings.Contains(text, current.ID) || (current.Alias != "" && !strings.Contains(text, current.Alias)) {
				t.Fatalf("missing identity %+v: %s", current, text)
			}
			if previous.ID != current.ID && previous.ID != "" && strings.Contains(text, previous.ID) {
				t.Fatalf("stale identity: %s", text)
			}
		}
	}
	items := i.sessionOpsDialog.items
	if items[len(items)-1].action != "copy-id" {
		t.Fatalf("missing copy action: %+v", items)
	}
	current = sessionIdentity{ID: "44444444-4444-4444-8444-444444444444"}
	i.copySessionID(context.Background(), func(_ context.Context, text string) error {
		if text != current.ID {
			t.Fatalf("copied stale ID: %q", text)
		}
		return nil
	})
}

func TestSessionIdentityDoesNotUseTransientAgentID(t *testing.T) {
	i := &Interactive{agent: &core.Agent{SessionID: "transient-routing-id"}}
	if got := i.timelineData().Session; got != (sessionIdentity{}) {
		t.Fatalf("no-session exposed a transient ID: %+v", got)
	}
}

func TestRenderSessionIdentityNarrow(t *testing.T) {
	session := sessionIdentity{ID: "11111111-1111-4111-8111-111111111111", Alias: "grüne-湖"}
	for _, width := range []int{1, 8, 20, 40, 100} {
		rows := renderSessionIdentity(tui.Theme{}, width, session)
		var text strings.Builder
		for _, row := range rows {
			plain := stripANSIBytes(row)
			if got := runewidth.StringWidth(plain); got > width {
				t.Fatalf("width=%d: row width=%d: %q", width, got, plain)
			}
			text.WriteString(plain)
		}
		if !strings.Contains(text.String(), session.ID) {
			t.Fatalf("width=%d: UUID truncated: %q", width, text.String())
		}
	}
	rows := renderSessionIdentity(tui.Theme{}, 100, sessionIdentity{ID: "synthetic", Alias: "bad\x1b]52;c;payload\a\n"})
	for _, row := range rows {
		if plain := stripANSIBytes(row); strings.ContainsAny(plain, "\x1b\a\n") {
			t.Fatalf("untrusted terminal controls: %q", plain)
		}
	}
}

func TestSessionCopyIDCommandWithoutSession(t *testing.T) {
	i := &Interactive{}
	i.runSlash(context.Background(), "/session copy-id")
	if !strings.Contains(i.statusErr, "no active session") || i.statusOK != "" {
		t.Fatalf("unexpected command result: %q, %q", i.statusOK, i.statusErr)
	}
}

func TestCopySessionID(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		err  error
	}{
		{name: "success", id: "full-session-uuid"},
		{name: "no session"},
		{name: "unavailable", id: "full-session-uuid", err: errors.New("clipboard unavailable")},
		{name: "cancelled", id: "full-session-uuid", err: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := &Interactive{cfg: InteractiveConfig{CurrentSessionIdentity: func() (string, string) { return tc.id, "alias-not-to-copy" }}}
			ctx := context.WithValue(context.Background(), struct{}{}, "synthetic context")
			called := false
			i.copySessionID(ctx, func(gotCtx context.Context, text string) error {
				called = true
				if text != tc.id || gotCtx != ctx {
					t.Fatalf("clipboard text=%q, wrong context=%v", text, gotCtx != ctx)
				}
				return tc.err
			})
			if called != (tc.id != "") {
				t.Fatalf("clipboard called=%v", called)
			}
			if tc.id != "" && tc.err == nil {
				if i.statusOK == "" || i.statusErr != "" {
					t.Fatalf("copy not successful: %q, %q", i.statusOK, i.statusErr)
				}
			} else if i.statusErr == "" || i.statusOK != "" {
				t.Fatalf("copy failure not reported: %q, %q", i.statusOK, i.statusErr)
			}
		})
	}
}
