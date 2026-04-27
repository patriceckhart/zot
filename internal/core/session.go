package core

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/internal/provider"
)

// Session is a JSONL-backed conversation transcript tied to a cwd.
type Session struct {
	ID     string
	Path   string
	Meta   SessionMeta
	writer *os.File
	buf    *bufio.Writer

	// freshFile is true when the file was created by NewSession (this
	// process owns it) and false when OpenSession reopened an existing
	// transcript. Used by Close() to delete the file if the run never
	// appended any messages — prevents a flood of empty session files
	// from sessions the user opens then exits without prompting.
	freshFile bool

	// messagesAppended counts AppendMessage calls. Combined with
	// freshFile it tells Close() whether the session left any content
	// worth keeping.
	messagesAppended int
}

// SessionMeta is written as the first line of every session file.
type SessionMeta struct {
	ID       string    `json:"id"`
	CWD      string    `json:"cwd"`
	Model    string    `json:"model"`
	Provider string    `json:"provider"`
	Started  time.Time `json:"started"`
	Version  string    `json:"version"`
	Title    string    `json:"title,omitempty"`

	// Parent is the ID of the session this one was forked from, or
	// empty for top-level sessions. The tree picker walks parents
	// upward and sibling files (same cwd dir, same parent ID)
	// laterally to render the branch topology.
	Parent string `json:"parent,omitempty"`
	// ForkPoint is the 0-indexed message position within the parent
	// transcript where this branch diverges. Messages 0..ForkPoint-1
	// are copied from the parent verbatim; the user's next turn on
	// the child session continues from there.
	ForkPoint int `json:"fork_point,omitempty"`
}

// sessionLine is the on-disk row type. Message is kept as a raw
// JSON message on reads (because Content is an interface slice that
// the default unmarshaler cannot reconstruct); it is written with a
// regular provider.Message value.
type sessionLine struct {
	Type       string            `json:"type"`
	Meta       *SessionMeta      `json:"meta,omitempty"`
	Message    *provider.Message `json:"message,omitempty"`
	Usage      *provider.Usage   `json:"usage,omitempty"`
	Cumulative *provider.Usage   `json:"cumulative,omitempty"`
}

type sessionLineHead struct {
	Type string `json:"type"`
}

// SessionsDir returns the per-cwd sessions directory under root.
func SessionsDir(root, cwd string) string {
	sum := sha256.Sum256([]byte(cwd))
	short := hex.EncodeToString(sum[:8])
	return filepath.Join(root, "sessions", short)
}

// NewSession creates and opens a new session file.
func NewSession(root, cwd, providerName, model, version string) (*Session, error) {
	dir := SessionsDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	id := uuid.NewString()
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().UTC().Format("20060102-150405"), id[:8])
	p := filepath.Join(dir, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID:        id,
		Path:      p,
		Meta:      SessionMeta{ID: id, CWD: cwd, Provider: providerName, Model: model, Started: time.Now().UTC(), Version: version},
		writer:    f,
		buf:       bufio.NewWriter(f),
		freshFile: true,
	}
	if err := s.writeLine(sessionLine{Type: "meta", Meta: &s.Meta}); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// SessionUsage returns the most recent cumulative usage row stored in
// a session file. Sessions append one usage row per completed turn; the
// latest row's cumulative field is the session total. Missing usage rows
// are valid for old/empty sessions and return the zero value.
func SessionUsage(path string) (provider.Usage, error) {
	f, err := os.Open(path)
	if err != nil {
		return provider.Usage{}, err
	}
	defer f.Close()

	var usage provider.Usage
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	for sc.Scan() {
		var head sessionLineHead
		if err := json.Unmarshal(sc.Bytes(), &head); err != nil || head.Type != "usage" {
			continue
		}
		var row struct {
			Cumulative provider.Usage `json:"cumulative"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err == nil {
			usage = row.Cumulative
		}
	}
	if err := sc.Err(); err != nil {
		return provider.Usage{}, err
	}
	return usage, nil
}

// OpenSession opens an existing session for appending.
func OpenSession(path string) (*Session, []provider.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	var meta SessionMeta
	var messages []provider.Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	for sc.Scan() {
		var head sessionLineHead
		if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
			continue
		}
		switch head.Type {
		case "meta":
			var row struct {
				Meta SessionMeta `json:"meta"`
			}
			if err := json.Unmarshal(sc.Bytes(), &row); err == nil {
				meta = row.Meta
			}
		case "message":
			if msg, err := hydrateMessage(sc.Bytes()); err == nil && len(msg.Content) > 0 {
				messages = append(messages, msg)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	messages = repairToolUseResultPairs(messages)
	out, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, nil, err
	}
	s := &Session{ID: meta.ID, Path: path, Meta: meta, writer: out, buf: bufio.NewWriter(out)}
	return s, messages, nil
}

// repairToolUseResultPairs walks a restored transcript and
// synthesises stub tool_result blocks for any assistant
// tool_use blocks that aren't paired with a matching result in
// the next message. Anthropic (and OpenAI via the responses API)
// reject any request whose transcript leaves a tool_use without
// its matching tool_result immediately after, with errors like:
//
//	messages.8: `tool_use` ids were found without `tool_result`
//	blocks immediately after
//
// Corruption gets into the transcript two ways we know of:
//
//   - Older zot builds that persisted the assistant tool_use row
//     before the tool_result row, then crashed between the two.
//   - Abort paths in older builds that didn't drop the mid-turn
//     assistant message cleanly.
//
// Rather than change runtime semantics (which would risk hiding a
// real bug), we scrub on load: any unmatched tool_use gets a stub
// tool_result injected as a RoleTool message so the next
// outbound request passes the provider's validity check. The stub
// reads "tool call was aborted; no result recorded." so the
// model can see what happened and decide whether to retry.
//
// Runs once per OpenSession call. No cost on the hot path.
func repairToolUseResultPairs(msgs []provider.Message) []provider.Message {
	if len(msgs) == 0 {
		return msgs
	}
	out := make([]provider.Message, 0, len(msgs)+2)
	for i, m := range msgs {
		out = append(out, m)
		if m.Role != provider.RoleAssistant {
			continue
		}
		// Collect tool_use ids in this assistant message.
		var ids []string
		for _, c := range m.Content {
			if tc, ok := c.(provider.ToolCallBlock); ok {
				ids = append(ids, tc.ID)
			}
		}
		if len(ids) == 0 {
			continue
		}
		// Look at the next message (if any) and collect tool_result
		// CallIDs it covers.
		have := map[string]bool{}
		if i+1 < len(msgs) && msgs[i+1].Role == provider.RoleTool {
			for _, c := range msgs[i+1].Content {
				if tr, ok := c.(provider.ToolResultBlock); ok {
					have[tr.CallID] = true
				}
			}
		}
		// Build stubs for any missing id.
		var stubs []provider.Content
		for _, id := range ids {
			if have[id] {
				continue
			}
			stubs = append(stubs, provider.ToolResultBlock{
				CallID:  id,
				Content: []provider.Content{provider.TextBlock{Text: "tool call was aborted; no result recorded."}},
				IsError: true,
			})
		}
		if len(stubs) == 0 {
			continue
		}
		// Merge into the next tool-role message if present,
		// otherwise insert a synthetic one right after the
		// assistant message. Merging keeps the tool-role row
		// count stable; inserting handles the common case where
		// no tool message was persisted at all.
		if i+1 < len(msgs) && msgs[i+1].Role == provider.RoleTool {
			msgs[i+1].Content = append(msgs[i+1].Content, stubs...)
			// We already appended m to out; the modified next
			// message will be appended on the following iteration.
			continue
		}
		out = append(out, provider.Message{
			Role:    provider.RoleTool,
			Content: stubs,
			Time:    m.Time,
		})
	}
	return out
}

// LatestSession returns the most recent session file for cwd, or "".
func LatestSession(root, cwd string) string {
	paths := ListSessions(root, cwd)
	if len(paths) == 0 {
		return ""
	}
	return paths[0]
}

// SessionSummary describes one on-disk session at a glance for UI pickers.
type SessionSummary struct {
	Path          string
	Started       time.Time
	Model         string
	Provider      string
	MessageCount  int
	FirstUserText string
	TotalCost     float64
	Title         string
}

// RenameSession updates the title field in the session's meta line.
// It rewrites the first line of the file (the meta line) with the
// updated title.
// RenameSession appends a rename line to the session file. This is
// safe even for the currently active session because it opens the
// file independently and appends (doesn't rewrite).
func RenameSession(path, title string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	line, _ := json.Marshal(map[string]string{"type": "rename", "title": title})
	line = append(line, '\n')
	_, err = f.Write(line)
	return err
}

// DescribeSessions returns lightweight summaries for every session in
// cwd, newest first. Parses only the first few lines and the last usage
// line so it's cheap to run on every dialog open.
func DescribeSessions(root, cwd string) []SessionSummary {
	paths := ListSessions(root, cwd)
	summaries := make([]SessionSummary, 0, len(paths))
	for _, p := range paths {
		summaries = append(summaries, describeSession(p))
	}
	return summaries
}

func describeSession(path string) SessionSummary {
	s := SessionSummary{Path: path}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	for sc.Scan() {
		var head sessionLineHead
		if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
			continue
		}
		switch head.Type {
		case "meta":
			var row struct {
				Meta SessionMeta `json:"meta"`
			}
			if err := json.Unmarshal(sc.Bytes(), &row); err == nil {
				s.Started = row.Meta.Started
				s.Model = row.Meta.Model
				s.Provider = row.Meta.Provider
				s.Title = row.Meta.Title
			}
		case "message":
			s.MessageCount++
			if s.FirstUserText == "" {
				s.FirstUserText = firstUserText(sc.Bytes())
			}
		case "rename":
			var row struct {
				Title string `json:"title"`
			}
			if err := json.Unmarshal(sc.Bytes(), &row); err == nil && row.Title != "" {
				s.Title = row.Title
			}
		case "usage":
			var row struct {
				Cumulative provider.Usage `json:"cumulative"`
			}
			if err := json.Unmarshal(sc.Bytes(), &row); err == nil {
				s.TotalCost = row.Cumulative.CostUSD
			}
		}
	}
	return s
}

func firstUserText(line []byte) string {
	var row struct {
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &row); err != nil {
		return ""
	}
	if row.Message.Role != "user" {
		return ""
	}
	for _, c := range row.Message.Content {
		if c.Text != "" {
			return c.Text
		}
	}
	return ""
}

// PruneEmptySessions deletes session files in cwd's session directory
// that contain only a meta line (no messages were ever appended).
// Cleans up the backlog of empty stubs created by old zot versions
// that wrote a meta line at NewSession time and never followed up.
// Errors are swallowed; the caller treats this as best-effort.
func PruneEmptySessions(root, cwd string) {
	dir := SessionsDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if sessionHasNoMessages(p) {
			_ = os.Remove(p)
		}
	}
}

// sessionHasNoMessages returns true when the file at path contains
// no lines of type "message". Meta-only / usage-only files count as
// empty. Used by PruneEmptySessions and the Describe path.
func sessionHasNoMessages(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	for sc.Scan() {
		var head sessionLineHead
		if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
			continue
		}
		if head.Type == "message" {
			return false
		}
	}
	return true
}

// ListSessions returns session file paths for cwd, newest first.
func ListSessions(root, cwd string) []string {
	dir := SessionsDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files
}

// AppendMessage writes a message to the session.
func (s *Session) AppendMessage(m provider.Message) error {
	if s == nil {
		return nil
	}
	if err := s.writeLine(sessionLine{Type: "message", Message: &m}); err != nil {
		return err
	}
	s.messagesAppended++
	return nil
}

// UpdateModel records a provider/model switch in the session file.
// The reader keeps the most recent meta entry, so the session resumes
// with the updated model.
func (s *Session) UpdateModel(providerName, model string) error {
	if s == nil {
		return nil
	}
	s.Meta.Provider = providerName
	s.Meta.Model = model
	return s.writeLine(sessionLine{Type: "meta", Meta: &s.Meta})
}

// AppendUsage writes a usage row to the session.
func (s *Session) AppendUsage(u, cum provider.Usage) error {
	if s == nil {
		return nil
	}
	return s.writeLine(sessionLine{Type: "usage", Usage: &u, Cumulative: &cum})
}

// Close flushes and closes the session file. If the session was
// freshly created in this process and never had any messages
// appended (the user opened zot, looked around, and exited without
// prompting), the file is deleted on close so the sessions list
// doesn't fill up with empty meta-only stubs.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	flushErr := s.buf.Flush()
	closeErr := s.writer.Close()
	if s.freshFile && s.messagesAppended == 0 {
		// Best-effort cleanup. We deliberately don't propagate the
		// remove error: if it fails (file already gone, perms changed)
		// the worst case is one stale empty file in the listing.
		_ = os.Remove(s.Path)
	}
	if flushErr != nil {
		return flushErr
	}
	return closeErr
}

func (s *Session) writeLine(row sessionLine) error {
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if _, err := s.buf.Write(b); err != nil {
		return err
	}
	if err := s.buf.WriteByte('\n'); err != nil {
		return err
	}
	return s.buf.Flush()
}

// ---- content (de)serialization ----
//
// provider.Content is an interface; encoding/json drops type information.
// We persist messages by reading the raw "message" object back and
// rebuilding Content from discriminated fields.

func hydrateMessage(lineBytes []byte) (provider.Message, error) {
	var row struct {
		Message struct {
			Role    provider.Role     `json:"role"`
			Content []json.RawMessage `json:"content"`
			Time    time.Time         `json:"time"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lineBytes, &row); err != nil {
		return provider.Message{}, err
	}
	msg := provider.Message{Role: row.Message.Role, Time: row.Message.Time}
	for _, raw := range row.Message.Content {
		var head struct {
			Text     string `json:"text"`
			MimeType string `json:"mime_type"`
			Data     []byte `json:"data"`
			ID       string `json:"id"`
			Name     string `json:"name"`
			CallID   string `json:"call_id"`
			// ToolCallBlock also has Arguments, ToolResultBlock has Content + IsError
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			continue
		}
		// Discriminate by presence of fields.
		switch {
		case head.Name != "" && head.ID != "":
			var tc struct {
				ID        string          `json:"id"`
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(raw, &tc)
			msg.Content = append(msg.Content, provider.ToolCallBlock{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		case head.CallID != "":
			var tr struct {
				CallID  string            `json:"call_id"`
				Content []json.RawMessage `json:"content"`
				IsError bool              `json:"is_error"`
			}
			_ = json.Unmarshal(raw, &tr)
			block := provider.ToolResultBlock{CallID: tr.CallID, IsError: tr.IsError}
			for _, c := range tr.Content {
				var inner struct {
					Text     string `json:"text"`
					MimeType string `json:"mime_type"`
					Data     []byte `json:"data"`
				}
				_ = json.Unmarshal(c, &inner)
				if inner.MimeType != "" {
					block.Content = append(block.Content, provider.ImageBlock{MimeType: inner.MimeType, Data: inner.Data})
				} else {
					block.Content = append(block.Content, provider.TextBlock{Text: inner.Text})
				}
			}
			msg.Content = append(msg.Content, block)
		case head.MimeType != "":
			msg.Content = append(msg.Content, provider.ImageBlock{MimeType: head.MimeType, Data: head.Data})
		default:
			msg.Content = append(msg.Content, provider.TextBlock{Text: head.Text})
		}
	}
	return msg, nil
}
