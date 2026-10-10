package continuous

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
)

// wsTestClient is a minimal masked WebSocket client for tests.
type wsTestClient struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialTestWebSocket(t *testing.T, url string, header http.Header) (*wsTestClient, *http.Response) {
	t.Helper()
	addr := strings.TrimPrefix(url, "http://")
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	// The key is the RFC 6455 section 1.3 example, so the accept value is
	// known.
	req := "GET / HTTP/1.1\r\nHost: " + addr + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n"
	for k, vs := range header {
		for _, v := range vs {
			req += k + ": " + v + "\r\n"
		}
	}
	if _, err := io.WriteString(conn, req+"\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &wsTestClient{conn: conn, br: br}, resp
}

func (c *wsTestClient) sendFrame(t *testing.T, op byte, payload []byte, masked bool) {
	t.Helper()
	head := []byte{0x80 | op}
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		head = append(head, maskBit|byte(n))
	case n <= 0xFFFF:
		head = append(head, maskBit|126, byte(n>>8), byte(n))
	default:
		head = append(head, maskBit|127)
		head = binary.BigEndian.AppendUint64(head, uint64(n))
	}
	body := append([]byte(nil), payload...)
	if masked {
		var mask [4]byte
		_, _ = rand.Read(mask[:])
		head = append(head, mask[:]...)
		for i := range body {
			body[i] ^= mask[i%4]
		}
	}
	if _, err := c.conn.Write(append(head, body...)); err != nil {
		t.Fatal(err)
	}
}

func (c *wsTestClient) send(t *testing.T, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	c.sendFrame(t, wsOpText, b, true)
}

// next reads one server frame. Server frames are never masked.
func (c *wsTestClient) next(t *testing.T) (byte, []byte) {
	t.Helper()
	var head [2]byte
	if _, err := io.ReadFull(c.br, head[:]); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if head[1]&0x80 != 0 {
		t.Fatal("server frame is masked")
	}
	n := uint64(head[1] & 0x7F)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			t.Fatal(err)
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(c.br, ext[:]); err != nil {
			t.Fatal(err)
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		t.Fatal(err)
	}
	return head[0] & 0x0F, payload
}

func (c *wsTestClient) response(t *testing.T) hostResponse {
	t.Helper()
	for {
		op, payload := c.next(t)
		if op != wsOpText {
			t.Fatalf("unexpected opcode %d payload %q", op, payload)
		}
		var resp hostResponse
		if err := json.Unmarshal(payload, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.Type == "response" {
			return resp
		}
	}
}

func startWebSocketHost(t *testing.T, origins []string) (*httptest.Server, string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := newTestRuntime(t)
	engine := echoEngine(&scriptedClient{steps: []scriptStep{{text: "hello from host"}}}, core.NewRegistry())
	host, err := NewHost(r, engine, HostOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx) }()
	token := "web-test-token-0123456789"
	server := &HostServer{Host: host, Engine: engine, Tokens: map[string]Role{token: RoleAdmin}}
	handler, err := server.WebSocketHandler(ctx, origins)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		cancel()
		srv.Close()
		<-done
	})
	return srv, token
}

func TestWebSocketHandlerRequiresTokens(t *testing.T) {
	server := &HostServer{}
	if _, err := server.WebSocketHandler(context.Background(), nil); err == nil {
		t.Fatal("websocket endpoint without tokens accepted")
	}
}

func TestWebSocketRoundTrip(t *testing.T) {
	srv, token := startWebSocketHost(t, nil)
	c, resp := dialTestWebSocket(t, srv.URL, nil)
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("handshake status %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept key %q", got)
	}
	c.send(t, map[string]any{"id": "1", "method": "hello", "params": map[string]string{"token": token}})
	if r := c.response(t); !r.Success {
		t.Fatalf("hello failed: %+v", r)
	}
	// Messages larger than 125 bytes use the extended length encoding in
	// both directions.
	workspace := strings.Repeat("w", 300)
	c.send(t, map[string]any{"id": "2", "method": "conversation.create", "params": map[string]string{"workspace": workspace}})
	if r := c.response(t); !r.Success || r.ID != "2" {
		t.Fatalf("create failed: %+v", r)
	}
	// Pings are answered with pongs carrying the same payload.
	c.sendFrame(t, wsOpPing, []byte("hi"), true)
	if op, payload := c.next(t); op != wsOpPong || string(payload) != "hi" {
		t.Fatalf("ping answered with op %d %q", op, payload)
	}
}

func TestWebSocketFragmentedMessage(t *testing.T) {
	srv, token := startWebSocketHost(t, nil)
	c, _ := dialTestWebSocket(t, srv.URL, nil)
	msg, _ := json.Marshal(map[string]any{"id": "1", "method": "hello", "params": map[string]string{"token": token}})
	half := len(msg) / 2
	// First fragment: text opcode without FIN; second: continuation with FIN.
	first := append([]byte{wsOpText, 0x80 | byte(half)}, 0, 0, 0, 0)
	first = append(first, msg[:half]...)
	last := append([]byte{0x80 | wsOpContinuation, 0x80 | byte(len(msg)-half)}, 0, 0, 0, 0)
	last = append(last, msg[half:]...)
	if _, err := c.conn.Write(append(first, last...)); err != nil {
		t.Fatal(err)
	}
	if r := c.response(t); !r.Success {
		t.Fatalf("fragmented hello failed: %+v", r)
	}
}

func TestWebSocketRejectsBadToken(t *testing.T) {
	srv, _ := startWebSocketHost(t, nil)
	c, _ := dialTestWebSocket(t, srv.URL, nil)
	c.send(t, map[string]any{"id": "1", "method": "hello", "params": map[string]string{"token": "wrong-token-0123456789"}})
	if r := c.response(t); r.Success || r.Code != "unauthorized" {
		t.Fatalf("bad token accepted: %+v", r)
	}
	c2, _ := dialTestWebSocket(t, srv.URL, nil)
	c2.send(t, map[string]any{"id": "1", "method": "conversation.list"})
	if r := c2.response(t); r.Success || r.Code != "unauthorized" {
		t.Fatalf("request before hello accepted: %+v", r)
	}
}

func TestWebSocketRejectsInvalidFrames(t *testing.T) {
	srv, _ := startWebSocketHost(t, nil)
	for name, tc := range map[string]struct {
		op      byte
		payload []byte
		masked  bool
		code    uint16
	}{
		"unmasked":  {op: wsOpText, payload: []byte(`{"id":"1","method":"hello"}`), code: 1002},
		"multiline": {op: wsOpText, payload: []byte("{\"id\":\"1\",\n\"method\":\"hello\"}"), masked: true, code: 1007},
		"binary":    {op: wsOpBinary, payload: []byte{1, 2, 3}, masked: true, code: 1003},
		"utf8":      {op: wsOpText, payload: []byte{0xff, 0xfe}, masked: true, code: 1007},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := dialTestWebSocket(t, srv.URL, nil)
			c.sendFrame(t, tc.op, tc.payload, tc.masked)
			op, payload := c.next(t)
			if op != wsOpClose || len(payload) < 2 || binary.BigEndian.Uint16(payload) != tc.code {
				t.Fatalf("got op %d payload %q, want close %d", op, payload, tc.code)
			}
		})
	}
}

func TestWebSocketClosePayload(t *testing.T) {
	srv, _ := startWebSocketHost(t, nil)
	for name, tc := range map[string]struct {
		payload []byte
		code    uint16
	}{
		"empty":           {code: 1000},
		"one byte":        {payload: []byte{3}, code: 1002},
		"reserved code":   {payload: binary.BigEndian.AppendUint16(nil, 1005), code: 1002},
		"tls only code":   {payload: binary.BigEndian.AppendUint16(nil, 1015), code: 1002},
		"undefined code":  {payload: binary.BigEndian.AppendUint16(nil, 2000), code: 1002},
		"out of range":    {payload: binary.BigEndian.AppendUint16(nil, 5000), code: 1002},
		"invalid reason":  {payload: []byte{3, 232, 0xff}, code: 1007},
		"normal reason":   {payload: append(binary.BigEndian.AppendUint16(nil, 1000), []byte("done")...), code: 1000},
		"private code":    {payload: binary.BigEndian.AppendUint16(nil, 4001), code: 4001},
		"registered code": {payload: binary.BigEndian.AppendUint16(nil, 3000), code: 3000},
	} {
		t.Run(name, func(t *testing.T) {
			c, _ := dialTestWebSocket(t, srv.URL, nil)
			c.sendFrame(t, wsOpClose, tc.payload, true)
			op, payload := c.next(t)
			if op != wsOpClose || len(payload) < 2 || binary.BigEndian.Uint16(payload) != tc.code {
				t.Fatalf("got op %d payload %q, want close %d", op, payload, tc.code)
			}
			if name == "normal reason" && string(payload[2:]) != "done" {
				t.Fatalf("close reason not echoed: %q", payload[2:])
			}
		})
	}
}

func TestWebSocketOriginAllowlist(t *testing.T) {
	srv, _ := startWebSocketHost(t, []string{"https://app.example"})
	_, resp := dialTestWebSocket(t, srv.URL, http.Header{"Origin": {"https://evil.example"}})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin got status %d", resp.StatusCode)
	}
	_, resp = dialTestWebSocket(t, srv.URL, http.Header{"Origin": {"https://App.example/"}})
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("allowed origin got status %d", resp.StatusCode)
	}
}

func TestWebSocketRejectsPlainHTTP(t *testing.T) {
	srv, _ := startWebSocketHost(t, nil)
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plain GET got %d", resp.StatusCode)
	}
}
