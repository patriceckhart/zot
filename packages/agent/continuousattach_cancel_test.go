package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"
)

func TestContinuousAttachCancellation(t *testing.T) {
	for _, method := range []string{"submission.wait", "conversation.watch"} {
		t.Run(method, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			waiting := make(chan struct{})
			serverDone := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					serverDone <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				dec, enc := json.NewDecoder(bufio.NewReader(conn)), json.NewEncoder(conn)
				for {
					var req struct {
						ID     string `json:"id"`
						Method string `json:"method"`
					}
					if err := dec.Decode(&req); err != nil {
						serverDone <- err
						return
					}
					if req.Method == method {
						close(waiting)
						_, err := io.Copy(io.Discard, conn)
						serverDone <- err
						return
					}
					data := map[string]any{"id": "id", "revision": 1}
					if err := enc.Encode(map[string]any{"type": "response", "id": req.ID, "success": true, "data": data}); err != nil {
						serverDone <- err
						return
					}
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			args := []string{"--address", ln.Addr().String(), "--workspace", "w"}
			if method == "submission.wait" {
				args = append(args, "hello")
			} else {
				args = append(args, "--follow")
			}
			done := make(chan error, 1)
			go func() { done <- runContinuousAttach(ctx, args, io.Discard) }()
			select {
			case <-waiting:
			case err := <-done:
				t.Fatalf("attach exited before waiting: %v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("attach never reached blocking method")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("attach did not detach on cancellation")
			}
			if err := <-serverDone; err != nil {
				t.Fatalf("connection not closed on detach: %v", err)
			}
		})
	}
}
