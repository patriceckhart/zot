//go:build windows

package continuous

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func localPipePair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "host.sock")
	ln, err := ListenLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := DialLocal(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	server, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for _, conn := range []net.Conn{client, server} {
		if err := conn.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
	}
	return client, server
}

func TestLocalPipeBufferedRoundTrip(t *testing.T) {
	client, server := localPipePair(t)
	for i := 0; i < 64; i++ {
		for direction, pair := range [][2]net.Conn{{client, server}, {server, client}} {
			writer, reader := pair[0], pair[1]
			payload := []byte(fmt.Sprintf("frame %d direction %d\n", i, direction))
			// Finish the write before starting the read, exercising immediate
			// overlapped completion rather than only ERROR_IO_PENDING.
			if n, err := writer.Write(payload); err != nil || n != len(payload) {
				t.Fatalf("write: %d, %v", n, err)
			}
			// An empty read must not report EOF or consume the queued frame.
			if n, err := reader.Read(nil); n != 0 || err != nil {
				t.Fatalf("empty read: %d, %v", n, err)
			}
			got := make([]byte, len(payload))
			if _, err := io.ReadFull(reader, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("frame: %q, want %q", got, payload)
			}
		}
	}
}

func TestLocalPipeReadSurvivesGC(t *testing.T) {
	client, server := localPipePair(t)
	payload := bytes.Repeat([]byte("pending pipe frame\n"), 128)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		got := make([]byte, len(payload))
		close(started)
		_, err := io.ReadFull(server, got)
		if err == nil && !bytes.Equal(got, payload) {
			err = fmt.Errorf("corrupted pipe frame")
		}
		done <- err
	}()
	<-started
	// Collection may shrink blocked goroutine stacks. Kernel-owned pointers
	// must remain stable until overlapped I/O completes.
	runtime.GC()
	if n, err := client.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("write: %d, %v", n, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipe read did not finish")
	}
}

func TestLocalPipeCloseJoinsRead(t *testing.T) {
	_, server := localPipePair(t)
	started := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		var buf [1]byte
		close(started)
		_, err := server.Read(buf[:])
		readDone <- err
	}()
	<-started
	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close did not join the pipe read")
	}
	select {
	case err := <-readDone:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("read after close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("close left the pipe read blocked")
	}
}
