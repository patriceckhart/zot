package continuous

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// WebSocket transport for the host protocol, so browser clients can talk to
// a host directly. It is a minimal RFC 6455 server on the standard library:
// one text message carries one protocol frame (a JSON object without the
// trailing newline). Binary messages, extensions, and subprotocols are not
// supported.

// MaxWebSocketMessage bounds one inbound message. It matches the line buffer
// of the newline transport.
const MaxWebSocketMessage = 1 << 20

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// websocketWriteTimeout bounds one outbound frame so a stalled browser
// cannot hold host memory indefinitely; the connection ends instead.
const websocketWriteTimeout = 30 * time.Second

// WebSocketHandler serves the host protocol over WebSocket upgrades. The
// server must have Tokens: a browser endpoint without authentication would
// let any web page a user visits drive the host. origins, when non-empty,
// restricts the Origin header (scheme://host[:port]) as a defense in depth;
// the token is the access check.
func (s *HostServer) WebSocketHandler(ctx context.Context, origins []string) (http.Handler, error) {
	if len(s.Tokens) == 0 {
		return nil, fmt.Errorf("websocket endpoint requires tokens")
	}
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[normalizeOrigin(o)] = true
	}
	limit := s.MaxConnections
	if limit <= 0 {
		limit = 256
	}
	slots := make(chan struct{}, limit)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(allowed) > 0 && !allowed[normalizeOrigin(r.Header.Get("Origin"))] {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			http.Error(w, "connection limit reached", http.StatusServiceUnavailable)
			return
		}
		defer func() { <-slots }()
		conn, err := upgradeWebSocket(w, r)
		if err != nil {
			return
		}
		s.ServeConn(ctx, conn)
	}), nil
}

func normalizeOrigin(o string) string {
	return strings.TrimRight(strings.ToLower(strings.TrimSpace(o)), "/")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// upgradeWebSocket performs the opening handshake. On failure it has
// already written an HTTP error.
func upgradeWebSocket(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	key := r.Header.Get("Sec-WebSocket-Key")
	if r.Method != http.MethodGet || !headerHasToken(r.Header, "Connection", "upgrade") || !headerHasToken(r.Header, "Upgrade", "websocket") || key == "" {
		w.Header().Set("Upgrade", "websocket")
		http.Error(w, "zot continuous host: WebSocket endpoint", http.StatusUpgradeRequired)
		return nil, errors.New("not a websocket request")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("unsupported websocket version")
	}
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		http.Error(w, "invalid Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("invalid key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusInternalServerError)
		return nil, errors.New("hijack unsupported")
	}
	netConn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + websocketGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	_ = netConn.SetDeadline(time.Time{})
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"); err != nil {
		netConn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		netConn.Close()
		return nil, err
	}
	return &wsConn{conn: netConn, br: rw.Reader}, nil
}

// wsConn adapts a server-side WebSocket to the newline-delimited stream
// ServeConn expects: each inbound text message is read as one line, and
// each written line is sent as one text message.
type wsConn struct {
	conn    net.Conn
	br      *bufio.Reader
	pending []byte // unread bytes of the current inbound message plus '\n'

	writeMu sync.Mutex
	wbuf    []byte // written bytes not yet terminated by '\n'
	closed  bool
}

const (
	wsOpContinuation = 0x0
	wsOpText         = 0x1
	wsOpBinary       = 0x2
	wsOpClose        = 0x8
	wsOpPing         = 0x9
	wsOpPong         = 0xA
)

func (c *wsConn) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		msg, err := c.readMessage()
		if err != nil {
			return 0, err
		}
		// A frame is a single JSON line. Embedded newlines would split it
		// into several requests, so they are refused.
		if bytes.IndexByte(msg, '\n') >= 0 {
			c.closeWith(1007, "message must be single-line JSON")
			return 0, io.EOF
		}
		c.pending = append(msg, '\n')
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// readMessage returns the next complete text message, answering control
// frames along the way.
func (c *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	started := false
	for {
		fin, op, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case wsOpPing:
			if err := c.writeFrame(wsOpPong, payload); err != nil {
				return nil, err
			}
			continue
		case wsOpPong:
			continue
		case wsOpClose:
			if len(payload) == 1 {
				c.closeWith(1002, "invalid close payload")
				return nil, io.EOF
			}
			if len(payload) >= 2 {
				code := binary.BigEndian.Uint16(payload)
				if !validWebSocketCloseCode(code) {
					c.closeWith(1002, "invalid close code")
					return nil, io.EOF
				}
				if !utf8.Valid(payload[2:]) {
					c.closeWith(1007, "invalid close reason")
					return nil, io.EOF
				}
				c.closeWith(code, string(payload[2:]))
			} else {
				c.closeWith(1000, "")
			}
			return nil, io.EOF
		case wsOpBinary:
			c.closeWith(1003, "binary messages are not supported")
			return nil, io.EOF
		case wsOpText:
			if started {
				c.closeWith(1002, "unexpected text frame")
				return nil, io.EOF
			}
			started = true
		case wsOpContinuation:
			if !started {
				c.closeWith(1002, "unexpected continuation frame")
				return nil, io.EOF
			}
		default:
			c.closeWith(1002, "unknown opcode")
			return nil, io.EOF
		}
		if len(msg)+len(payload) > MaxWebSocketMessage {
			c.closeWith(1009, "message too large")
			return nil, io.EOF
		}
		msg = append(msg, payload...)
		if fin {
			if !utf8.Valid(msg) {
				c.closeWith(1007, "invalid UTF-8")
				return nil, io.EOF
			}
			return msg, nil
		}
	}
}

// Protocol-defined codes exclude reserved codes that cannot appear on the
// wire. Codes 3000 through 4999 are registered or private application codes.
func validWebSocketCloseCode(code uint16) bool {
	return (code >= 1000 && code <= 1014 && code != 1004 && code != 1005 && code != 1006) ||
		(code >= 3000 && code <= 4999)
}

func (c *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var head [2]byte
	if _, err = io.ReadFull(c.br, head[:]); err != nil {
		return
	}
	fin = head[0]&0x80 != 0
	if head[0]&0x70 != 0 {
		c.closeWith(1002, "reserved bits set")
		return false, 0, nil, io.EOF
	}
	op = head[0] & 0x0F
	if head[1]&0x80 == 0 {
		// Clients must mask every frame.
		c.closeWith(1002, "unmasked client frame")
		return false, 0, nil, io.EOF
	}
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	if op >= wsOpClose && (length > 125 || !fin) {
		c.closeWith(1002, "invalid control frame")
		return false, 0, nil, io.EOF
	}
	if length > MaxWebSocketMessage {
		c.closeWith(1009, "message too large")
		return false, 0, nil, io.EOF
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return
}

// Write sends every complete line as one text message. ServeConn writes a
// whole frame including its newline in one call.
func (c *wsConn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	c.wbuf = append(c.wbuf, p...)
	for {
		i := bytes.IndexByte(c.wbuf, '\n')
		if i < 0 {
			break
		}
		if i > 0 {
			if err := c.writeFrameLocked(wsOpText, c.wbuf[:i]); err != nil {
				return 0, err
			}
		}
		c.wbuf = c.wbuf[i+1:]
	}
	return len(p), nil
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	return c.writeFrameLocked(op, payload)
}

func (c *wsConn) writeFrameLocked(op byte, payload []byte) error {
	frame := make([]byte, 0, len(payload)+10)
	frame = append(frame, 0x80|op)
	switch n := len(payload); {
	case n < 126:
		frame = append(frame, byte(n))
	case n <= 0xFFFF:
		frame = append(frame, 126, byte(n>>8), byte(n))
	default:
		frame = append(frame, 127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(n))
	}
	frame = append(frame, payload...)
	_ = c.conn.SetWriteDeadline(time.Now().Add(websocketWriteTimeout))
	_, err := c.conn.Write(frame)
	return err
}

// closeWith sends a close frame (best effort) and closes the connection.
func (c *wsConn) closeWith(code uint16, reason string) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return
	}
	payload := binary.BigEndian.AppendUint16(nil, code)
	if len(reason) > 123 {
		reason = reason[:123]
	}
	payload = append(payload, reason...)
	_ = c.writeFrameLocked(wsOpClose, payload)
	c.closed = true
	c.conn.Close()
}

func (c *wsConn) Close() error {
	c.closeWith(1000, "")
	return nil
}
