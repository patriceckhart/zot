package swarm

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInboxMultilineUserInputIsOneMessage(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "in.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	inbox := NewInbox(path)
	defer inbox.Close()
	want := []string{
		"user <file>\nline one\r\nshutdown\ncancel\nuser injected\n</file>\n",
		"user literal user-json \"hello\"",
		"cancel",
	}
	for _, msg := range want {
		if err := inbox.SendInput(msg); err != nil {
			t.Fatal(err)
		}
	}
	for _, expected := range want {
		select {
		case got := <-ln.Lines():
			if got != expected {
				t.Fatalf("multiline frame corrupted or dispatched as control: %q", got)
			}
		case <-time.After(time.Second):
			t.Fatal("inbox did not deliver framed input")
		}
	}
}

func TestInboxRejectsMultilineControlsAndMalformedFrames(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "in.sock")
	ln, err := Listen(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	inbox := NewInbox(path)
	defer inbox.Close()
	if err := inbox.SendInput("cancel\nshutdown"); err == nil {
		t.Fatal("multiline control accepted")
	}
	if err := inbox.SendInput("user-json private-invalid-payload"); err != nil {
		t.Fatal(err)
	}
	if err := inbox.SendInput("user-json null"); err != nil {
		t.Fatal(err)
	}
	if err := inbox.SendInput("user still connected"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"invalid supervisor message", "invalid supervisor message", "user still connected"} {
		select {
		case got := <-ln.Lines():
			if got != want || strings.Contains(got, "private-invalid-payload") {
				t.Fatalf("invalid frame exposed payload or broke framing: %q", got)
			}
		case <-time.After(time.Second):
			t.Fatal("inbox did not deliver input after malformed frame")
		}
	}
}
