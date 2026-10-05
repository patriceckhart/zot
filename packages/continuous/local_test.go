package continuous

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The private local endpoint (Unix socket or Windows named pipe) carries the
// host protocol: a client dials by store path, authenticates, creates a
// conversation, and a second client is served concurrently. Closing the
// listener ends Serve and refuses later dials.
func TestLocalEndpointServesHostProtocol(t *testing.T) {
	server, _, _, _ := newHostServer(t, map[string]Role{"submit-secret-0123456789": RoleSubmit})
	dir, err := os.MkdirTemp("", "zl")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "h.sock")
	ln, err := ListenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(ln.Addr().String(), `\\.\pipe\`) || ln.Addr().String() != LocalEndpointName(path) {
			t.Fatalf("pipe address: %s", ln.Addr())
		}
	} else {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("socket mode: %v %v", info, err)
		}
	}
	// A second pipe listener on the same name must fail. On Unix a second
	// ListenLocal replaces a stale socket file by design, so the check is
	// Windows only.
	if runtime.GOOS == "windows" {
		if second, err := ListenLocal(path); err == nil {
			second.Close()
			t.Fatal("second pipe listener accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, ln) }()

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	c1, err := Dial(dialCtx, path, "submit-secret-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := Dial(dialCtx, LocalEndpointName(path), "submit-secret-0123456789")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	var conv Conversation
	if err := c1.CallInto(dialCtx, "conversation.create", map[string]any{"workspace": "w"}, &conv); err != nil {
		t.Fatal(err)
	}
	var again Conversation
	if err := c2.CallInto(dialCtx, "conversation.create", map[string]any{"workspace": "w"}, &again); err != nil || again.ID != conv.ID {
		t.Fatalf("second client: %+v %v", again, err)
	}
	// Watches stream over the endpoint too.
	watch, err := c2.Watch(dialCtx, conv.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sub Submission
	if err := c1.CallInto(dialCtx, "conversation.submit", map[string]any{"id": conv.ID, "content": "hi", "request_id": "r1"}, &sub); err != nil {
		t.Fatal(err)
	}
	select {
	case cm, ok := <-watch:
		if !ok || len(cm.Operations) == 0 {
			t.Fatalf("watch frame: %v %v", cm, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no commit on watch")
	}
	// Wrong token is refused over the same transport.
	if _, err := Dial(dialCtx, path, "wrong-token-0123456789"); err == nil {
		t.Fatal("wrong token accepted")
	}
	cancel()
	if err := <-served; err == nil {
		t.Fatal("serve returned nil after cancel")
	}
	failCtx, failCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer failCancel()
	if _, err := DialLocal(failCtx, path); err == nil {
		t.Fatal("dial after close succeeded")
	}
}

func TestLocalEndpointNameIsStable(t *testing.T) {
	a := LocalEndpointName(filepath.Join("store", "host.sock"))
	b := LocalEndpointName(filepath.Join("store", "host.sock"))
	if a != b || a == "" {
		t.Fatalf("unstable endpoint name: %q %q", a, b)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(a, `\\.\pipe\zot-continuous-`) || LocalEndpointName(a) != a {
			t.Fatalf("pipe name: %q", a)
		}
		if LocalEndpointName(filepath.Join("other", "host.sock")) == a {
			t.Fatal("different stores share a pipe name")
		}
	}
	if !IsLocalAddress(a) || IsLocalAddress("127.0.0.1:9000") || IsLocalAddress("[::1]:9000") || !IsLocalAddress("/tmp/h.sock") || !IsLocalAddress(`\\.\pipe\x`) {
		t.Fatal("local address classification")
	}
}
