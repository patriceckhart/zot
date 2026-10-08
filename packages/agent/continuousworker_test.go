package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContinuousWorkerStdio(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello worker"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	_ = os.WriteFile(outside, []byte("nope"), 0o644)

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runContinuousWorker(ctx, []string{"--root", "proj=" + root, "--ledger", filepath.Join(t.TempDir(), "ledger.jsonl")}, inR, outW)
		outW.Close()
	}()
	reader := bufio.NewReader(outR)
	call := func(id, method string, params any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(params)
		b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": json.RawMessage(raw)})
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var resp map[string]any
		_ = json.Unmarshal(line, &resp)
		return resp
	}
	hello := call("1", "hello", map[string]any{"token": ""})
	if hello["success"] != true || !strings.Contains(string(mustJSON(hello["data"])), "read") {
		t.Fatalf("hello %v", hello)
	}
	exec := func(id, repo, tool string, args any) map[string]any {
		inner, _ := json.Marshal(args)
		wargs, _ := json.Marshal(workerArgs{Repo: repo, Args: inner})
		return call(id, "tool.execute", map[string]any{"operation_key": "op-" + id, "run_id": "r", "conversation_id": "c",
			"call_id": id, "epoch": 1, "tool": tool, "args": json.RawMessage(wargs)})
	}
	res := exec("2", "proj", "read", map[string]any{"path": "a.txt"})
	if !strings.Contains(string(mustJSON(res)), "hello worker") {
		t.Fatalf("read %v", res)
	}
	res = exec("3", "proj", "read", map[string]any{"path": outside})
	if !strings.Contains(string(mustJSON(res)), "is_error\":true") {
		t.Fatalf("jail escape allowed: %v", res)
	}
	res = exec("4", "other", "read", map[string]any{"path": "a.txt"})
	if !strings.Contains(string(mustJSON(res)), "not shared") {
		t.Fatalf("unknown repo allowed: %v", res)
	}
	inW.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop on EOF")
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
