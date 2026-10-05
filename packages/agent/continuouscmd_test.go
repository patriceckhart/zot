package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestContinuousOptions(t *testing.T) {
	valid := [][]string{
		nil, {"--help"}, {"import", "source", "--store", "store"},
		{"verify", "--store", "store"}, {"verify", "--help"},
		{"backup", "archive", "--store", "store"},
		{"restore", "archive", "--store", "new-store", "--durability", "process"},
		{"verify-backup", "archive"},
		{"export", "--store", "store", "id", "--format", "session", "--output", "out"},
		{"conversations", "--store", "store", "--after", "id", "--limit", "1000", "--durability", "process"},
		{"status", "--help"}, {"import", "--store", "store", "--", "--source"},
	}
	for _, args := range valid {
		if _, err := parseContinuousOptions(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	invalid := [][]string{
		{"serve"}, {"attach", "id"}, {"verify"}, {"status"}, {"import", "--store", "store"},
		{"import", "one", "two", "--store", "store"}, {"status", "id", "--store", "store"},
		{"status", "--store"}, {"status", "--store", ""}, {"status", "--store", "--durability", "process"},
		{"status", "--store", "store", "--unknown"}, {"status", "--store", "store", "--output", "out"},
		{"status", "--store", "store", "--limit", "1"}, {"export", "id", "--store", "store", "--format", "lossless"},
		{"status", "--store", "store", "--durability", "memory"},
		{"conversations", "--store", "store", "--limit", "0"}, {"conversations", "--store", "store", "--limit", "1001"},
		{"conversations", "--store", "store", "--limit", "invalid"},
		{"status", "--store", "one", "--store", "two"},
		{"verify", "--store", "store", "--durability", "process"},
		{"verify", "--store", "store", "--output", "out"},
		{"verify", "--store", "store", "--limit", "1"},
		{"verify", "--store", "store", "id"},
		{"backup", "--store", "store"}, {"restore", "archive"}, {"verify-backup"},
		{"verify-backup", "archive", "--store", "store"},
		{"verify-backup", "archive", "--durability", "process"},
		{"backup", "archive", "--store", "store", "--output", "out"},
	}
	for _, args := range invalid {
		if _, err := parseContinuousOptions(args); err == nil {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
}

func TestContinuousCLIImportExport(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	storePath := filepath.Join(home, "store")
	session, err := core.NewSessionAtPath(filepath.Join(home, "source.jsonl"), "/synthetic/cwd", "synthetic", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "private synthetic input"}}}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	opts := []string{"--store", storePath, "--durability", "process"}
	run := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := runContinuous(ctx, append(args, opts...), &out); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	imported := run("import", session.Path)
	if strings.Contains(imported, "private synthetic input") {
		t.Fatal("import acknowledgement included transcript content")
	}
	var result struct {
		Conversation continuous.Conversation `json:"conversation"`
	}
	if err := json.Unmarshal([]byte(imported), &result); err != nil {
		t.Fatal(err)
	}
	if result.Conversation.ID != session.ID {
		t.Fatal("source ID lost")
	}
	var repeated struct {
		Conversation continuous.Conversation `json:"conversation"`
	}
	if err := json.Unmarshal([]byte(run("import", session.Path)), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.Conversation.ID != result.Conversation.ID {
		t.Fatal("duplicate import")
	}
	var status continuous.Status
	if err := json.Unmarshal([]byte(run("status")), &status); err != nil {
		t.Fatal(err)
	}
	if status.ExecutionEnabled || status.Capabilities.Durability != storage.Process || !status.Capabilities.BoundedHistoryMemory || !status.Capabilities.Indexed {
		t.Fatalf("dishonest status: %+v", status)
	}
	listed := run("conversations", "--limit", "1")
	if !strings.Contains(listed, session.ID) || strings.Contains(listed, "private synthetic input") {
		t.Fatalf("conversation list: %s", listed)
	}
	empty := run("conversations", "--after", session.ID)
	if !strings.Contains(empty, `"conversations":[]`) {
		t.Fatalf("pagination: %s", empty)
	}
	exported := run("export", session.ID, "--format", "session")
	if !bytes.Equal([]byte(exported), source) {
		t.Fatal("CLI projection changed valid legacy rows")
	}
	target := filepath.Join(home, "output.zotsession")
	if out := run("export", session.ID, "--output", target); out != "" {
		t.Fatalf("file export polluted stdout: %s", out)
	}
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, source) {
		t.Fatal("file export differs")
	}
	var out bytes.Buffer
	if err := runContinuous(ctx, append([]string{"export", session.ID, "--output", target}, opts...), &out); err == nil {
		t.Fatal("existing output overwritten")
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed export changed existing file")
	}
	original, err := os.ReadFile(session.Path)
	if err != nil || !bytes.Equal(original, source) {
		t.Fatal("import changed source session")
	}
}

func TestContinuousCLIDoesNotCreateInspectionStore(t *testing.T) {
	for _, command := range []string{"status", "conversations", "export"} {
		t.Run(command, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "missing")
			args := []string{command, "--store", path, "--durability", "process"}
			if command == "export" {
				args = append(args, "id")
			}
			if err := runContinuous(context.Background(), args, &bytes.Buffer{}); err == nil {
				t.Fatal("missing store accepted")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("inspection created a store")
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := runContinuous(context.Background(), args, &bytes.Buffer{}); err == nil {
				t.Fatal("empty directory accepted as a store")
			}
			contents, err := os.ReadDir(path)
			if err != nil || len(contents) != 0 {
				t.Fatal("inspection initialized empty directory")
			}
		})
	}
}
func TestContinuousCLIRejectsActiveWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s, err := journal.Open(context.Background(), path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := runContinuous(context.Background(), []string{"status", "--store", path, "--durability", "process"}, &bytes.Buffer{}); err == nil {
		t.Fatal("inspection bypassed writer lock")
	}
}
func TestContinuousRouterAndHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"status"}, {"-p", "continuous"}} {
		if handled, err := runContinuousCommand(args); handled || err != nil {
			t.Fatalf("unrelated command handled: %v", args)
		}
	}
	if handled, err := runContinuousCommand([]string{"continuous", "unsupported"}); !handled || err == nil {
		t.Fatal("continuous command not routed")
	}
	var out bytes.Buffer
	if err := runContinuous(context.Background(), []string{"--help"}, &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"import", "export", "status", "conversations", "verify", "backup", "restore", "verify-backup", "run", "--resume", "never repeated unless the tool declares replay safe"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q", want)
		}
	}
	helpPath := filepath.Join(t.TempDir(), "help.txt")
	file, err := os.Create(helpPath)
	if err != nil {
		t.Fatal(err)
	}
	printHelp(file, "test")
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	help, err := os.ReadFile(helpPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(help), "zot continuous --help") {
		t.Fatal("top-level help missing continuous")
	}
}
