package core

import (
	"encoding/json"
	"sync"
	"testing"
)

type recordingConfirmer struct {
	mu      sync.Mutex
	calls   []string
	replies []ConfirmDecision
	idx     int
}

func (r *recordingConfirmer) Confirm(toolName, preview string) ConfirmDecision {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, toolName+"/"+preview)
	if r.idx >= len(r.replies) {
		return ConfirmDecision{Allow: false, Reason: "no reply queued"}
	}
	d := r.replies[r.idx]
	r.idx++
	return d
}

func TestConfirmGateNilAllowsEverything(t *testing.T) {
	var g *ConfirmGate
	allow, reason, args := g.Check("bash", "rm -rf /")
	if !allow || reason != "" || args != nil {
		t.Fatalf("nil gate should allow, got allow=%v reason=%q args=%s", allow, reason, args)
	}
}

func TestConfirmGateNilInnerRefuses(t *testing.T) {
	g := NewConfirmGate(nil)
	allow, reason, _ := g.Check("bash", "ls")
	if allow {
		t.Fatal("gate with nil inner should refuse")
	}
	if reason == "" {
		t.Fatal("refusal must carry a reason for the model to learn from")
	}
}

func TestConfirmGateAllowOnce(t *testing.T) {
	rc := &recordingConfirmer{replies: []ConfirmDecision{
		{Allow: true},
		{Allow: true},
	}}
	g := NewConfirmGate(rc)
	if allow, _, _ := g.Check("bash", "ls"); !allow {
		t.Fatal("call 1 should allow")
	}
	if allow, _, _ := g.Check("bash", "ls"); !allow {
		t.Fatal("call 2 should allow")
	}
	// Two calls, two confirmer invocations (no remember).
	if len(rc.calls) != 2 {
		t.Errorf("want 2 confirmer calls, got %d", len(rc.calls))
	}
}

func TestConfirmGateRememberTool(t *testing.T) {
	rc := &recordingConfirmer{replies: []ConfirmDecision{
		{Allow: true, RememberTool: true},
	}}
	g := NewConfirmGate(rc)

	// First call prompts and remembers.
	if allow, _, _ := g.Check("bash", "ls"); !allow {
		t.Fatal("call 1 should allow")
	}
	// Second call short-circuits; confirmer must NOT be invoked.
	if allow, _, _ := g.Check("bash", "pwd"); !allow {
		t.Fatal("call 2 should allow from memory")
	}
	// Different tool still prompts.
	rc.replies = append(rc.replies, ConfirmDecision{Allow: false, Reason: "no"})
	if allow, reason, _ := g.Check("read", "foo.txt"); allow || reason != "no" {
		t.Errorf("different tool should re-prompt; got allow=%v reason=%q", allow, reason)
	}
	if len(rc.calls) != 2 {
		t.Errorf("want 2 confirmer calls (bash+read), got %d: %v", len(rc.calls), rc.calls)
	}
}

func TestConfirmGateRememberAll(t *testing.T) {
	rc := &recordingConfirmer{replies: []ConfirmDecision{
		{Allow: true, RememberAll: true},
	}}
	g := NewConfirmGate(rc)
	if allow, _, _ := g.Check("bash", "ls"); !allow {
		t.Fatal("call 1 should allow")
	}
	// From now on everything short-circuits.
	if allow, _, _ := g.Check("read", "foo.txt"); !allow {
		t.Fatal("call 2 should allow")
	}
	if allow, _, _ := g.Check("write", "bar.txt"); !allow {
		t.Fatal("call 3 should allow")
	}
	if len(rc.calls) != 1 {
		t.Errorf("want 1 confirmer call (remember-all short-circuits the rest), got %d", len(rc.calls))
	}
}

func TestConfirmGateRefuseSurfacesReason(t *testing.T) {
	rc := &recordingConfirmer{replies: []ConfirmDecision{
		{Allow: false, Reason: "do not run sudo"},
	}}
	g := NewConfirmGate(rc)
	allow, reason, _ := g.Check("bash", "sudo rm -rf /")
	if allow || reason != "do not run sudo" {
		t.Errorf("want block + reason, got allow=%v reason=%q", allow, reason)
	}
}

func TestConfirmGateRefuseEmptyReasonGetsDefault(t *testing.T) {
	rc := &recordingConfirmer{replies: []ConfirmDecision{
		{Allow: false},
	}}
	g := NewConfirmGate(rc)
	_, reason, _ := g.Check("bash", "x")
	if reason == "" {
		t.Fatal("empty reason must be replaced with a sensible default")
	}
}

func TestConfirmGateAllowAllRuntime(t *testing.T) {
	rc := &recordingConfirmer{replies: []ConfirmDecision{
		{Allow: false, Reason: "no"},
	}}
	g := NewConfirmGate(rc)
	// Refuse first call
	if allow, _, _ := g.Check("bash", "ls"); allow {
		t.Fatal("call 1 should refuse")
	}
	// User types /yolo
	g.AllowAll()
	// All subsequent calls allowed without prompting.
	if allow, _, _ := g.Check("bash", "rm -rf tmp"); !allow {
		t.Fatal("after AllowAll, should allow")
	}
	if allow, _, _ := g.Check("read", "x"); !allow {
		t.Fatal("after AllowAll, should allow any tool")
	}
	if len(rc.calls) != 1 {
		t.Errorf("want 1 confirmer call (before AllowAll), got %d", len(rc.calls))
	}
}

func TestBuildPreview(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string
	}{
		{"bash command", `{"command":"ls -la"}`, "ls -la"},
		{"path", `{"path":"/tmp/x.txt"}`, "/tmp/x.txt"},
		{"file_path", `{"file_path":"a.go"}`, "a.go"},
		{"url", `{"url":"https://example.com"}`, "https://example.com"},
		{"truncation", `{"command":"` + string(make([]byte, 200)) + `"}`, ""},
		{"unparseable", `{not json`, `{not json`},
		{"empty", ``, ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := BuildPreview(json.RawMessage(c.args), 50)
			if c.name == "truncation" && !hasEllipsis(got) {
				t.Errorf("%s: expected ellipsis truncation, got %q", c.name, got)
				return
			}
			if c.want != "" && got != c.want {
				t.Errorf("%s: want %q, got %q", c.name, c.want, got)
			}
		})
	}
}

func hasEllipsis(s string) bool {
	return len(s) > 0 && s[len(s)-len("\u2026"):] == "\u2026"
}
