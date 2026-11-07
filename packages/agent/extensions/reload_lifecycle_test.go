package extensions

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extproto"
)

func TestReloadReplaysActiveSession(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	for _, state := range []string{"active", "legacy", "closed", "missing"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			script := `import json, sys

def emit(frame):
    print(json.dumps(frame), flush=True)

emit({"type":"hello","name":"replay","version":"1","capabilities":["events","commands"]})
emit({"type":"subscribe","events":["session_start","session_end"]})
emit({"type":"register_command","name":"seen","description":"observed events"})
emit({"type":"ready"})
events = []
for line in sys.stdin:
    frame = json.loads(line)
    if frame["type"] == "event":
        events.append(frame)
    elif frame["type"] == "command_invoked":
        emit({"type":"command_response","id":frame["id"],"action":"display","display":json.dumps(events)})
    elif frame["type"] == "shutdown":
        emit({"type":"shutdown_ack"})
        break
`
			if err := os.WriteFile(filepath.Join(dir, "ext.py"), []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			manifest, err := json.Marshal(Manifest{Name: "replay", Exec: python, Args: []string{filepath.Join(dir, "ext.py")}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "extension.json"), manifest, 0o600); err != nil {
				t.Fatal(err)
			}
			m := New(t.TempDir(), dir, "test", "", "", nil)
			t.Cleanup(func() { m.Stop(time.Second) })
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if errs := m.LoadExplicit(ctx, []string{dir}); len(errs) != 0 {
				t.Fatal(errs)
			}
			m.WaitForReady(time.Second)
			alias := "tidy-birch"
			if state == "legacy" {
				alias = ""
			}
			if state != "missing" {
				m.StartSessionWithAlias("session-one", alias, dir, "")
			}
			if state == "closed" {
				m.EndSession("user_exit")
			}
			var previous uint64 = 1
			for reload := 0; reload < 2; reload++ {
				stats := m.Reload(ctx, time.Second)
				if len(stats.Errors) != 0 || stats.Loaded != 1 {
					t.Fatalf("reload: %+v", stats)
				}
				// Starting the same session must remain idempotent after replay.
				if state == "active" || state == "legacy" {
					m.StartSessionWithAlias("session-one", alias, dir, "")
				}
				response, err := m.Invoke(ctx, "seen", "", time.Second)
				if err != nil {
					t.Fatal(err)
				}
				var events []extproto.EventFromHost
				if err := json.Unmarshal([]byte(response.Display), &events); err != nil {
					t.Fatal(err)
				}
				if state == "closed" || state == "missing" {
					if len(events) != 0 {
						t.Fatalf("unexpected replay: %+v", events)
					}
					continue
				}
				if len(events) != 1 {
					t.Fatalf("want one replay, got %+v", events)
				}
				event := events[0]
				if event.Event != "session_start" || event.SessionID != "session-one" || event.SessionAlias != alias || event.CWD != dir || event.Sequence <= previous {
					t.Fatalf("invalid replay: %+v", event)
				}
				previous = event.Sequence
			}
		})
	}
}
