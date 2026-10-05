package continuous

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func legacyFixture(t *testing.T, id string, messages ...provider.Message) string {
	t.Helper()
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	if err := enc.Encode(map[string]any{"type": "meta", "meta": core.SessionMeta{ID: id, CWD: "/synthetic/project", Provider: "synthetic", Model: "first", Parent: "parent-id", ForkPoint: 2}}); err != nil {
		t.Fatal(err)
	}
	for _, msg := range messages {
		if err := enc.Encode(map[string]any{"type": "message", "message": msg}); err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}
func userMessage(text string) provider.Message {
	return provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: text}}}
}
func toolMessage(id string) provider.Message {
	return provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.ToolCallBlock{ID: id, Name: "bash", Arguments: json.RawMessage(`{"command":"synthetic"}`)}}}
}
func decodeExport(t *testing.T, data []byte) []json.RawMessage {
	t.Helper()
	var rows []json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var row json.RawMessage
		err := dec.Decode(&row)
		if err == io.EOF {
			return rows
		}
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
}
func reopenProjection(t *testing.T, data []byte) (*core.Session, []provider.Message) {
	t.Helper()
	// Check the raw projection before OpenSession's compatibility repair can
	// mask a missing result or malformed row in our exporter.
	_, _, repairs, err := parseSessionRows(context.Background(), data)
	if err != nil || len(repairs) != 0 {
		t.Fatalf("unpaired raw projection: %v %v", repairs, err)
	}
	path := filepath.Join(t.TempDir(), "projection.jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, messages, err := core.OpenSession(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, messages
}

func TestSessionImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	r := newTestRuntime(t)
	messages := []provider.Message{
		userMessage("review"), toolMessage("call"),
		{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call", Content: []provider.Content{provider.TextBlock{Text: "tool output", Format: "markdown"}, provider.ImageBlock{MimeType: "image/png", Data: []byte("synthetic image")}}}}},
		{Role: provider.RoleAssistant, AddedToolNames: []string{"deferred"}, Meta: map[string]string{"state": "synthetic"}, Content: []provider.Content{provider.ReasoningBlock{ID: "reason", Summary: "summary", Encrypted: "synthetic blob"}, provider.TextBlock{Text: "answer", ThoughtSignature: "synthetic signature"}}},
	}
	fixture := legacyFixture(t, "source-id", messages...)
	fixture += `{"type":"usage","usage":{"input_tokens":10,"output_tokens":5},"cumulative":{"input_tokens":10,"output_tokens":5,"cost_usd":0.01}}` + "\n"
	fixture += `{"type":"meta","meta":{"id":"source-id","cwd":"/synthetic/project","provider":"synthetic","model":"second","parent":"parent-id","fork_point":2},"extra":"preserve"}` + "\n"
	fixture += `{"type":"application_record","payload":{"future":"retained"}}` + "\n"
	before, _ := r.Snapshot(ctx)
	c, err := r.ImportSession(ctx, strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "source-id" || c.Config.Model != "second" || c.QueueSequence != 0 {
		t.Fatalf("import configuration: %+v", c)
	}
	commits, err := r.Scan(ctx, before.Revision(), 10)
	if err != nil || len(commits) != 1 {
		t.Fatalf("atomic import: %v %v", commits, err)
	}
	info, err := r.SessionImport(ctx, c.ID)
	if err != nil || info.ExecutionCheckpoints != "unavailable" || len(info.Repairs) != 0 || info.Meta.Parent != "parent-id" {
		t.Fatalf("provenance: %+v %v", info, err)
	}
	var out bytes.Buffer
	if err := r.ExportSession(ctx, c.ID, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), []byte(fixture)) {
		t.Fatalf("valid rows did not round-trip verbatim\n%s", out.String())
	}
	session, restored := reopenProjection(t, out.Bytes())
	if session.Meta.ID != c.ID || session.Meta.Parent != "parent-id" || session.Meta.ForkPoint != 2 || session.Meta.Model != "second" {
		t.Fatalf("legacy meta: %+v", session.Meta)
	}
	if !reflect.DeepEqual(messages, restored) {
		t.Fatalf("content changed: %#v", restored)
	}
	cumulative, err := core.SessionUsage(session.Path)
	if err != nil || cumulative.CostUSD != 0.01 || cumulative.InputTokens != 10 {
		t.Fatalf("usage: %+v %v", cumulative, err)
	}
}

func TestSessionMissingResultsRemainErrors(t *testing.T) {
	for _, mode := range []string{"tail", "merge", "compaction", "server"} {
		t.Run(mode, func(t *testing.T) {
			r := newTestRuntime(t)
			ctx := context.Background()
			assistant := toolMessage("missing")
			var fixture string
			switch mode {
			case "merge":
				assistant.Content = append(assistant.Content, provider.ToolCallBlock{ID: "done", Name: "read"})
				fixture = legacyFixture(t, "source", userMessage("input"), assistant, provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "done", Content: []provider.Content{provider.TextBlock{Text: "known output"}}}}})
			case "compaction":
				fixture = legacyFixture(t, "source", userMessage("old"))
				b, _ := json.Marshal(map[string]any{"type": "compaction", "messages": []provider.Message{userMessage("summary"), assistant}})
				fixture += string(b) + "\n"
			case "server":
				call := assistant.Content[0].(provider.ToolCallBlock)
				call.Server = true
				assistant.Content[0] = call
				fixture = legacyFixture(t, "source", userMessage("input"), assistant)
			default:
				fixture = legacyFixture(t, "source", userMessage("input"), assistant)
			}
			c, err := r.ImportSession(ctx, strings.NewReader(fixture))
			if err != nil {
				t.Fatal(err)
			}
			info, err := r.SessionImport(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := r.ExportSession(ctx, c.ID, &out); err != nil {
				t.Fatal(err)
			}
			_, restored := reopenProjection(t, out.Bytes())
			if mode == "server" {
				if len(info.Repairs) != 0 || len(restored) != 2 {
					t.Fatal("server call acquired synthetic local result")
				}
				return
			}
			if len(info.Repairs) != 1 || len(info.Repairs[0].CallIDs) != 1 || info.Repairs[0].CallIDs[0] != "missing" {
				t.Fatalf("repair notice: %+v", info.Repairs)
			}
			results := restored[len(restored)-1].Content
			found := false
			for _, block := range results {
				result := block.(provider.ToolResultBlock)
				if result.CallID == "missing" {
					found = true
					if !result.IsError || !strings.Contains(result.Content[0].(provider.TextBlock).Text, "interrupted") {
						t.Fatalf("invented result: %+v", result)
					}
				}
			}
			if !found {
				t.Fatal("missing tool pairing")
			}
			snap, _ := r.Snapshot(ctx)
			entries, _ := snap.Page("entry/"+c.ID+"/", "", 100)
			for _, rec := range entries {
				var e Entry
				if err := json.Unmarshal(rec.Value, &e); err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(e.LegacyRow, []byte("interrupted")) {
					t.Fatal("original source rewritten")
				}
			}
		})
	}
}

func TestSessionImportRejectsInvalidAtomically(t *testing.T) {
	meta := legacyFixture(t, "source")
	for _, fixture := range []string{
		"", "not-json", `{"type":"message"}`, `{"type":"meta","meta":null}`,
		meta + "{broken\n", meta + `{"type":"message","message":null}`,
		meta + `{"type":"message","message":{"role":"assistant","content":[{"id":"call"}]}}`,
		meta + `{"type":"message","message":{"role":"tool","content":[{"call_id":"orphan","content":[{"text":"private payload"}]}]}}`,
		meta + `{"type":"compaction","messages":"broken"}`,
		meta + `{"type":"usage","usage":{"input_tokens":"broken"}}`,
		meta + `{"type":"meta","meta":{"id":"different"}}`,
		meta + `{"type":"message","message":{"role":"user","time":"invalid","content":[{"text":"hello"}]}}`,
		meta + `{"type":"message","message":{"role":"user","content":[{"text":123}]}}`,
		legacyFixture(t, "source", toolMessage("call")) + `{"type":"message","message":{"role":"tool","content":[{"call_id":"call","content":[{"text":123}]}]}}`,
		legacyFixture(t, "../unsafe", userMessage("text")),
		legacyFixture(t, "source", toolMessage("call"), provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call", Content: []provider.Content{}}, provider.ToolResultBlock{CallID: "call", Content: []provider.Content{}}}}),
	} {
		t.Run(fmtTestName(fixture), func(t *testing.T) {
			r := newTestRuntime(t)
			before, _ := r.Snapshot(context.Background())
			if _, err := r.ImportSession(context.Background(), strings.NewReader(fixture)); err == nil {
				t.Fatal("invalid import accepted")
			} else if strings.Contains(err.Error(), "private payload") {
				t.Fatal("payload leaked in error")
			}
			after, _ := r.Snapshot(context.Background())
			if after.Revision() != before.Revision() {
				t.Fatal("failed import published")
			}
			rows, _ := after.Page("conversation/", "", 10)
			if len(rows) != 0 {
				t.Fatal("partial import")
			}
		})
	}
}
func fmtTestName(s string) string {
	if len(s) > 80 {
		return s[len(s)-80:]
	}
	return s
}

func TestSessionImportDedupAndEntrySequence(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	fixture := legacyFixture(t, "source", userMessage("historical"))
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := r.ImportSession(ctx, strings.NewReader(fixture)); errs <- err }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	c, _ := r.Conversation(ctx, "source")
	if c.EntrySequence != 2 || c.QueueSequence != 0 {
		t.Fatalf("import sequence: %+v", c)
	}
	if _, err := r.ImportSession(ctx, strings.NewReader(legacyFixture(t, "source", userMessage("conflict")))); !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("ID collision: %v", err)
	}
	s, err := r.Submit(ctx, c.ID, "actor", "next", "new input")
	if err != nil || s.Sequence != 1 {
		t.Fatalf("submission: %+v %v", s, err)
	}
	var out bytes.Buffer
	if err := r.ExportSession(ctx, c.ID, &out); err != nil {
		t.Fatal(err)
	}
	_, restored := reopenProjection(t, out.Bytes())
	if len(restored) != 2 || restored[0].Content[0].(provider.TextBlock).Text != "historical" || restored[1].Content[0].(provider.TextBlock).Text != "new input" {
		t.Fatal("admission overwrote imported history")
	}
}

func TestSessionImportLimitsAndCancellation(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	before, _ := r.Snapshot(ctx)
	if _, err := r.ImportSession(ctx, strings.NewReader(strings.Repeat("x", MaxSessionImportBytes+1))); err == nil {
		t.Fatal("oversized input accepted")
	}
	rows := legacyFixture(t, "many") + strings.Repeat(`{"type":"application"}`+"\n", MaxSessionImportRows)
	if _, err := r.ImportSession(ctx, strings.NewReader(rows)); err == nil {
		t.Fatal("oversized row count accepted")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.ImportSession(cancelled, strings.NewReader(legacyFixture(t, "source"))); !errors.Is(err, context.Canceled) {
		t.Fatalf("import cancellation: %v", err)
	}
	after, _ := r.Snapshot(ctx)
	if after.Revision() != before.Revision() {
		t.Fatal("rejected import changed state")
	}
}

func TestSessionImportRestartAndMissingID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	open := func() *Runtime {
		s, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
		if err != nil {
			t.Fatal(err)
		}
		r, err := New(s)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	fixture := legacyFixture(t, "", userMessage("input"))
	r := open()
	first, err := r.ImportSession(ctx, strings.NewReader(fixture))
	if err != nil || first.ID == "" {
		t.Fatalf("missing ID: %+v %v", first, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r = open()
	defer r.Close()
	again, err := r.ImportSession(ctx, strings.NewReader(fixture))
	if err != nil || first.ID != again.ID {
		t.Fatalf("restart dedup: %+v %v", again, err)
	}
	var out bytes.Buffer
	if err := r.ExportSession(ctx, first.ID, &out); err != nil {
		t.Fatal(err)
	}
	s, _ := reopenProjection(t, out.Bytes())
	if s.ID != first.ID {
		t.Fatal("generated import ID missing from projection")
	}
}

func TestSessionExportUsesOneSnapshot(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	c, _ := r.OpenRoot(ctx, "workspace", AgentConfig{})
	if _, err := r.Submit(ctx, c.ID, "actor", "one", "first"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	writer := &callbackWriter{out: &out, callback: func() {
		if _, err := r.Submit(ctx, c.ID, "actor", "two", "later"); err != nil {
			t.Fatal(err)
		}
	}}
	if err := r.ExportSession(ctx, c.ID, writer); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out.Bytes(), []byte("later")) {
		t.Fatal("export mixed revisions")
	}
	_, restored := reopenProjection(t, out.Bytes())
	if len(restored) != 1 {
		t.Fatalf("export length: %d", len(restored))
	}
	failure := errors.New("synthetic writer failure")
	if err := r.ExportSession(ctx, c.ID, errorWriter{failure}); !errors.Is(err, failure) {
		t.Fatalf("writer error: %v", err)
	}
}

type callbackWriter struct {
	out      io.Writer
	callback func()
}

func (w *callbackWriter) Write(b []byte) (int, error) {
	if w.callback != nil {
		fn := w.callback
		w.callback = nil
		fn()
	}
	return w.out.Write(b)
}

func TestSessionEmptyCompactionAndReusedCallIDs(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	result := provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "reused"}}}
	fixture := legacyFixture(t, "source", userMessage("old"), toolMessage("reused"), result, toolMessage("reused"), result)
	fixture += `{"type":"compaction"}` + "\n"
	fixture += `{"type":"message","message":{"role":"assistant","content":null}}` + "\n"
	c, err := r.ImportSession(ctx, strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.ExportSession(ctx, c.ID, &out); err != nil {
		t.Fatal(err)
	}
	if len(decodeExport(t, out.Bytes())) != 8 {
		t.Fatal("empty history records dropped")
	}
	_, messages := reopenProjection(t, out.Bytes())
	if len(messages) != 0 {
		t.Fatal("empty compaction did not reset context")
	}
	info, _ := r.SessionImport(ctx, c.ID)
	if len(info.Repairs) != 0 {
		t.Fatal("reused IDs mistaken for missing results")
	}
	c, err = r.Configure(ctx, c.ID, c.Revision, AgentConfig{Provider: "changed", Model: "new-model"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := r.ExportSession(ctx, c.ID, &out); err != nil {
		t.Fatal(err)
	}
	s, _ := reopenProjection(t, out.Bytes())
	if s.Meta.Provider != "changed" || s.Meta.Model != "new-model" || s.ID != c.ID {
		t.Fatalf("configured projection: %+v", s.Meta)
	}
}

func TestSessionAdmissionOldEntrySequence(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	c, _ := r.OpenRoot(ctx, "workspace", AgentConfig{})
	for _, text := range []string{"one", "two"} {
		if _, err := r.Submit(ctx, c.ID, "actor", text, text); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := r.Snapshot(ctx)
	c, _ = conversation(snap, c.ID)
	c.EntrySequence = 0 // Reproduce records created before the field was introduced.
	if err := r.commit(ctx, snap, "test", record("conversation/"+c.ID, c)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, c.ID, "actor", "three", "three"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.ExportSession(ctx, c.ID, &out); err != nil {
		t.Fatal(err)
	}
	_, messages := reopenProjection(t, out.Bytes())
	if len(messages) != 3 {
		t.Fatal("old entry counter overwrote committed history")
	}
}

func TestSessionEmptyMessagesDoNotBreakPairing(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	result := provider.Message{Role: provider.RoleTool, Content: []provider.Content{provider.ToolResultBlock{CallID: "call", Content: []provider.Content{provider.TextBlock{Text: "known output"}}}}}
	fixture := legacyFixture(t, "source", toolMessage("call"), provider.Message{Role: provider.RoleUser}, result)
	c, err := r.ImportSession(ctx, strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := r.ExportSession(ctx, c.ID, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != fixture {
		t.Fatal("empty source row changed")
	}
	_, messages := reopenProjection(t, out.Bytes())
	if len(messages) != 2 {
		t.Fatal("empty row changed provider context")
	}
	info, err := r.SessionImport(ctx, c.ID)
	if err != nil || len(info.Repairs) != 0 {
		t.Fatalf("invented interruption: %+v %v", info, err)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }
