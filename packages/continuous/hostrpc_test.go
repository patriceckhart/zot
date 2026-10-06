package continuous

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
)

type hostClient struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
	next   int
	// pushed collects events (frames without an id) seen while waiting for
	// responses, so tests can inspect watch deliveries.
	pushed []map[string]any
}

func dialHost(t *testing.T, server *HostServer) *hostClient {
	t.Helper()
	client, serverSide := net.Pipe()
	go server.ServeConn(context.Background(), serverSide)
	c := &hostClient{t: t, conn: client, reader: bufio.NewReader(client)}
	t.Cleanup(func() { client.Close() })
	return c
}

func (c *hostClient) send(method string, params any) string {
	c.next++
	id := fmt.Sprint("r", c.next)
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
	return id
}

func (c *hostClient) read() map[string]any {
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(line, &frame); err != nil {
		c.t.Fatalf("frame: %v %s", err, line)
	}
	return frame
}

// call sends a request and returns its response, collecting pushed frames.
func (c *hostClient) call(method string, params any) map[string]any {
	id := c.send(method, params)
	for {
		frame := c.read()
		if frame["id"] == id && frame["type"] == "response" {
			return frame
		}
		c.pushed = append(c.pushed, frame)
	}
}

func (c *hostClient) must(method string, params any) map[string]any {
	resp := c.call(method, params)
	if resp["success"] != true {
		c.t.Fatalf("%s failed: %v", method, resp)
	}
	data, _ := resp["data"].(map[string]any)
	return data
}

func newHostServer(t *testing.T, tokens map[string]Role) (*HostServer, *Runtime, *echoClient, context.CancelFunc) {
	t.Helper()
	r, err := New(newMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	client := &echoClient{delay: time.Millisecond}
	engine := echoEngine(client, core.NewRegistry())
	host, err := NewHost(r, engine, HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go host.Run(ctx)
	t.Cleanup(func() { cancel(); r.Close() })
	return &HostServer{Host: host, Engine: engine, Tokens: tokens, Version: "test"}, r, client, cancel
}

func TestHostProtocolAuthAndRoles(t *testing.T) {
	server, _, _, _ := newHostServer(t, map[string]Role{"read-secret": RoleRead, "submit-secret": RoleSubmit, "admin-secret": RoleAdmin})
	// No hello: refused and disconnected.
	c := dialHost(t, server)
	resp := c.call("runtime.status", nil)
	if resp["success"] != false || resp["code"] != "unauthorized" {
		t.Fatalf("missing hello: %v", resp)
	}
	// Wrong token.
	c = dialHost(t, server)
	if resp := c.call("hello", map[string]any{"token": "nope"}); resp["code"] != "unauthorized" {
		t.Fatalf("wrong token: %v", resp)
	}
	// Read role cannot submit or abort.
	reader := dialHost(t, server)
	if data := reader.must("hello", map[string]any{"token": "read-secret"}); data["role"] != "read" {
		t.Fatalf("hello: %v", data)
	}
	admin := dialHost(t, server)
	admin.must("hello", map[string]any{"token": "admin-secret"})
	conv := admin.must("conversation.create", map[string]any{"workspace": "w", "config": AgentConfig{}})
	id := conv["id"].(string)
	if resp := reader.call("conversation.submit", map[string]any{"id": id, "content": "x"}); resp["code"] != "forbidden" {
		t.Fatalf("read role submitted: %v", resp)
	}
	if resp := reader.call("conversation.create", map[string]any{"workspace": "w2"}); resp["code"] != "forbidden" {
		t.Fatalf("read role created: %v", resp)
	}
	reader.must("conversation.snapshot", map[string]any{"id": id, "limit": 10})
	submitter := dialHost(t, server)
	submitter.must("hello", map[string]any{"token": "submit-secret"})
	submitter.must("conversation.submit", map[string]any{"id": id, "content": "hello"})
	if resp := submitter.call("conversation.abort", map[string]any{"id": id}); resp["code"] != "forbidden" {
		t.Fatalf("submit role aborted: %v", resp)
	}
	if resp := admin.call("bogus.method", nil); resp["success"] != false {
		t.Fatalf("unknown method: %v", resp)
	}
	if resp := admin.call("usage.get", map[string]any{"id": "missing"}); resp["code"] != "not_found" {
		t.Fatalf("not found code: %v", resp)
	}
	// Error payloads never include transcript content.
	if resp := admin.call("conversation.snapshot", map[string]any{"id": id, "limit": 5000}); resp["success"] != false || strings.Contains(fmt.Sprint(resp), "hello") {
		t.Fatalf("snapshot limit: %v", resp)
	}
}

func TestTwoClientsSteerAndObserveOneConversation(t *testing.T) {
	server, r, _, _ := newHostServer(t, nil)
	a := dialHost(t, server)
	b := dialHost(t, server)
	conv := a.must("conversation.create", map[string]any{"workspace": "shared", "config": AgentConfig{Model: "echo"}})
	id := conv["id"].(string)
	// b watches from the snapshot revision; a submits.
	snap := b.must("conversation.snapshot", map[string]any{"id": id, "limit": 100})
	rev := uint64(snap["revision"].(float64))
	watchID := b.send("conversation.watch", map[string]any{"id": id, "after": rev})
	if ack := b.read(); ack["id"] != watchID || ack["success"] != true {
		t.Fatalf("watch ack: %v", ack)
	}
	sub := a.must("conversation.submit", map[string]any{"id": id, "content": "from a", "request_id": "a-1"})
	subID := sub["id"].(string)
	// Same request id from b with identical content: same submission.
	again := b.must("conversation.submit", map[string]any{"id": id, "content": "from a", "request_id": "a-1"})
	if again["id"] != subID {
		t.Fatalf("request id dedup across clients: %v vs %v", again["id"], subID)
	}
	// Different content with the same request id: duplicate_key.
	if resp := b.call("conversation.submit", map[string]any{"id": id, "content": "different", "request_id": "a-1"}); resp["code"] != "duplicate_key" {
		t.Fatalf("payload conflict: %v", resp)
	}
	settled := a.must("submission.wait", map[string]any{"id": subID})
	if settled["state"] != "answered" {
		t.Fatalf("settled: %v", settled)
	}
	// b receives commits for the conversation and can rebuild the entries.
	deadline := time.Now().Add(5 * time.Second)
	entries := map[string]bool{}
	for time.Now().Before(deadline) {
		var frames []map[string]any
		frames = append(frames, b.pushed...)
		b.pushed = nil
		b.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if line, err := b.reader.ReadBytes('\n'); err == nil {
			var f map[string]any
			json.Unmarshal(line, &f)
			frames = append(frames, f)
		}
		for _, f := range frames {
			if f["type"] != "commit" || f["watch_id"] != watchID {
				continue
			}
			commit := f["commit"].(map[string]any)
			for _, op := range commit["operations"].([]any) {
				m := op.(map[string]any)
				if key, _ := m["key"].(string); strings.HasPrefix(key, "entry/"+id+"/") {
					entries[key] = true
				}
			}
		}
		if len(entries) >= 3 {
			break
		}
	}
	// user, context, and assistant entries.
	if len(entries) != 3 {
		t.Fatalf("watcher saw %d entries", len(entries))
	}
	// Two clients configure the same revision: one conflict, no lost update.
	cur, _ := r.Conversation(context.Background(), id)
	okResp := a.call("conversation.configure", map[string]any{"id": id, "expected_revision": cur.Revision, "config": AgentConfig{Model: "a-model"}})
	conflict := b.call("conversation.configure", map[string]any{"id": id, "expected_revision": cur.Revision, "config": AgentConfig{Model: "b-model"}})
	if okResp["success"] != true || conflict["success"] != false || conflict["code"] != "conflict" {
		t.Fatalf("configure race: ok=%v conflict=%v", okResp, conflict)
	}
	final, _ := r.Conversation(context.Background(), id)
	if final.Config.Model != "a-model" {
		t.Fatalf("lost update: %+v", final.Config)
	}
	// Reject-when-busy and abort through the protocol.
	b.must("watch.cancel", map[string]any{"watch_id": watchID})
	b.must("conversation.reset", map[string]any{"id": id, "handoff": "continue"})
	usage := a.must("usage.get", map[string]any{"id": id})
	if usage["known"].(float64) < 1 {
		t.Fatalf("usage: %v", usage)
	}
	plan := a.must("recovery.preview", nil)
	if plan["blocked"].(float64) != 0 {
		t.Fatalf("plan: %v", plan)
	}
	if resp := a.call("conversation.abort", map[string]any{"id": id}); resp["code"] != "not_found" {
		t.Fatalf("abort idle: %v", resp)
	}
	status := a.must("runtime.status", nil)
	if _, ok := status["status"]; !ok {
		t.Fatalf("status: %v", status)
	}
}

func TestHostWatchCursorExpiredAndLimits(t *testing.T) {
	server, r, _, _ := newHostServer(t, nil)
	server.MaxWatches = 1
	c := dialHost(t, server)
	conv := c.must("conversation.create", map[string]any{"workspace": "w"})
	id := conv["id"].(string)
	cur, _ := r.Snapshot(context.Background())
	// A cursor in the future is rejected with cursor_expired semantics.
	wid := c.send("conversation.watch", map[string]any{"id": id, "after": cur.Revision() + 100})
	var sawError bool
	for i := 0; i < 3 && !sawError; i++ {
		f := c.read()
		if f["id"] == wid && f["success"] == false && f["code"] == "cursor_expired" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("future cursor accepted")
	}
	first := c.send("conversation.watch", map[string]any{"id": id, "after": cur.Revision()})
	if ack := c.read(); ack["id"] != first || ack["success"] != true {
		t.Fatalf("first watch: %v", ack)
	}
	if resp := c.call("conversation.watch", map[string]any{"id": id, "after": cur.Revision()}); resp["success"] != false || !strings.Contains(resp["error"].(string), "limit") {
		t.Fatalf("watch limit: %v", resp)
	}
	c.must("watch.cancel", map[string]any{"watch_id": first})
	// After cancel the slot is free again.
	second := c.send("conversation.watch", map[string]any{"id": id, "after": cur.Revision()})
	deadline := time.Now().Add(5 * time.Second)
	for {
		f := c.read()
		if f["id"] == second && f["success"] == true {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second watch never acknowledged")
		}
	}
	if resp := c.call("conversation.watch", map[string]any{"id": id}); resp["success"] != false || !strings.Contains(resp["error"].(string), "limit") {
		t.Fatalf("third watch with one active: %v", resp)
	}
}

// conversation.create with an owner creates an owned child once per (owner,
// key), inheriting the parent's configuration unless one is given, so a
// supervisor that retries creation attaches to the same child.
func TestHostCreateOwnedConversation(t *testing.T) {
	server, r, _, _ := newHostServer(t, map[string]Role{"submit-secret": RoleSubmit})
	c := dialHost(t, server)
	c.must("hello", map[string]any{"token": "submit-secret"})
	root := c.must("conversation.create", map[string]any{"workspace": "w", "config": map[string]any{"model": "parent-model"}})
	rootID := root["id"].(string)
	owner := map[string]any{"conversation_id": rootID, "id": "swarm/agent-1"}
	first := c.must("conversation.create", map[string]any{"owner": owner, "key": "agent-1"})
	second := c.must("conversation.create", map[string]any{"owner": owner, "key": "agent-1"})
	if first["id"] != second["id"] || first["id"] == rootID {
		t.Fatalf("owned creation not idempotent: %v %v", first, second)
	}
	child, err := r.Conversation(context.Background(), first["id"].(string))
	if err != nil || child.Owner == nil || child.Owner.ID != "swarm/agent-1" || child.Owner.ConversationID != rootID || child.Config.Model != "parent-model" {
		t.Fatalf("child: %+v %v", child, err)
	}
	other := c.must("conversation.create", map[string]any{"owner": owner, "key": "agent-2", "config": map[string]any{"model": "child-model"}})
	otherConv, err := r.Conversation(context.Background(), other["id"].(string))
	if err != nil || otherConv.Config.Model != "child-model" {
		t.Fatalf("configured child: %+v %v", otherConv, err)
	}
	if resp := c.call("conversation.create", map[string]any{"owner": owner}); resp["success"] != false {
		t.Fatalf("owner without key accepted: %v", resp)
	}
	if resp := c.call("conversation.create", map[string]any{"owner": map[string]any{"conversation_id": "missing", "id": "x"}, "key": "k"}); resp["success"] != false || resp["code"] != "not_found" {
		t.Fatalf("unknown owner conversation: %v", resp)
	}
}
