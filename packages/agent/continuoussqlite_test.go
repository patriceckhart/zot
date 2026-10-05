package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage/sqlite"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// --backend sqlite creates a SQLite store; later commands detect it without
// the flag, an explicit mismatching backend is refused, and the offline
// commands (status, check-state, export, search) work on it. Journal-only
// commands refuse a sqlite store.
func TestContinuousSQLiteBackend(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "synthetic-key")
	ctx := context.Background()
	server := &fakeChatServer{script: []string{textChunk("hello from sqlite")}}
	srv := httptest.NewServer(http.HandlerFunc(server.handler))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "store")
	var out bytes.Buffer
	args := []string{"say hello", "--store", store, "--backend", "sqlite", "--durability", "process", "--request-id", "req-1", "--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--no-ext", "--no-skills"}
	if err := runContinuousRun(ctx, args, &out); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "hello from sqlite") {
		t.Fatalf("answer: %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(store, sqlite.DatabaseFile)); err != nil {
		t.Fatalf("database not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "boundary")); err == nil {
		t.Fatal("journal files created for a sqlite store")
	}
	// Resume without the flag: the backend is detected. The identical
	// request ID returns the answered submission without new requests.
	out.Reset()
	server.script = nil
	if err := runContinuousRun(ctx, []string{"say hello", "--store", store, "--durability", "process", "--request-id", "req-1", "--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--no-ext", "--no-skills", "--json"}, &out); err != nil {
		t.Fatalf("resume: %v\n%s", err, out.String())
	}
	if len(server.requests) != 1 || !strings.Contains(out.String(), `"state":"answered"`) {
		t.Fatalf("retry executed work: %d %s", len(server.requests), out.String())
	}
	// A mismatching explicit backend is refused.
	if err := runContinuousRun(ctx, []string{"again", "--store", store, "--backend", "journal", "--durability", "process", "--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--no-ext", "--no-skills"}, &out); err == nil || !strings.Contains(err.Error(), "sqlite store, not journal") {
		t.Fatalf("mismatch accepted: %v", err)
	}
	for _, cmd := range [][]string{{"status"}, {"check-state"}, {"conversations"}, {"search", "--text", "hello"}} {
		out.Reset()
		cmdArgs := append(cmd, "--store", store, "--durability", "process")
		if cmd[0] == "search" {
			id := listFirstConversation(t, ctx, store)
			cmdArgs = append([]string{"search", id, "--text", "hello"}, "--store", store, "--durability", "process")
		}
		if err := runContinuous(ctx, cmdArgs, &out); err != nil {
			t.Fatalf("%s: %v", cmd[0], err)
		}
		if cmd[0] == "status" && !strings.Contains(out.String(), `"bounded_history_memory":true`) {
			t.Fatalf("status capabilities: %s", out.String())
		}
		if cmd[0] == "check-state" && !strings.Contains(out.String(), `"valid":true`) {
			t.Fatalf("check-state: %s", out.String())
		}
		if cmd[0] == "search" && !strings.Contains(out.String(), "hello from sqlite") {
			t.Fatalf("search: %s", out.String())
		}
	}
	// Journal-only commands refuse the store.
	for _, cmd := range []string{"verify", "format"} {
		if err := runContinuous(ctx, []string{cmd, "--store", store}, &out); err == nil {
			t.Fatalf("%s accepted a sqlite store", cmd)
		}
	}
	if err := runContinuous(ctx, []string{"backup", filepath.Join(t.TempDir(), "a"), "--store", store, "--durability", "process"}, &out); err == nil {
		t.Fatal("backup accepted a sqlite store")
	}
	if err := runContinuous(ctx, []string{"status", "--store", store, "--backend", "sqlite", "--durability", "process"}, &out); err == nil {
		t.Fatal("--backend accepted for an inspection command")
	}

	// Import into a new sqlite store and export back out.
	home := t.TempDir()
	session, err := core.NewSessionAtPath(filepath.Join(home, "source.jsonl"), "/synthetic/cwd", "synthetic", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "imported synthetic input"}}}); err != nil {
		t.Fatal(err)
	}
	session.Close()
	imported := filepath.Join(home, "imported")
	out.Reset()
	if err := runContinuous(ctx, []string{"import", session.Path, "--store", imported, "--backend", "sqlite", "--durability", "process"}, &out); err != nil {
		t.Fatalf("import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(imported, sqlite.DatabaseFile)); err != nil {
		t.Fatal("import did not create a sqlite store")
	}
	out.Reset()
	if err := runContinuous(ctx, []string{"export", session.ID, "--store", imported, "--durability", "process"}, &out); err != nil {
		t.Fatalf("export: %v", err)
	}
	if !strings.Contains(out.String(), "imported synthetic input") {
		t.Fatalf("export content: %s", out.String())
	}
}

func listFirstConversation(t *testing.T, ctx context.Context, store string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runContinuous(ctx, []string{"conversations", "--store", store, "--durability", "process"}, &out); err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Conversations []struct {
			ID string `json:"id"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal(out.Bytes(), &listing); err != nil || len(listing.Conversations) == 0 {
		t.Fatalf("listing: %s %v", out.String(), err)
	}
	return listing.Conversations[0].ID
}
