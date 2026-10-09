package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestClipboardCopySequence(t *testing.T) {
	text := "synthetic 界\n\x1b]52;c;injected\a"
	seq := ClipboardCopySequence(text)
	payload := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\x07")
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || string(decoded) != text {
		t.Fatal("OSC 52 did not safely encode selected text")
	}
}

func TestWriteClipboardTextCommandsStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test helper is a shell script")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "clipboard-test")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n/bin/cat > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	output := filepath.Join(dir, "copied")
	text := "synthetic clipboard ä界\n'$(false)'\n"
	if err := writeClipboardTextCommands(context.Background(), text,
		clipboardTextCommand{name: "missing-command"},
		clipboardTextCommand{name: "clipboard-test", args: []string{output}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || string(got) != text {
		t.Fatalf("stdin copy mismatch, error = %v", err)
	}
}

func TestWriteClipboardTextUnavailableAndCancelled(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if err := writeClipboardTextCommands(context.Background(), "synthetic",
		clipboardTextCommand{name: "missing-command"}); !errors.Is(err, errClipboardCommandUnavailable) {
		t.Fatalf("unavailable error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeClipboardTextCommands(ctx, "synthetic"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestWriteClipboardTextRemoteDoesNotUseLocalClipboard(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "synthetic")
	if err := WriteClipboardText(context.Background(), "synthetic"); err == nil {
		t.Fatal("SSH copy unexpectedly targeted local clipboard")
	}
}
