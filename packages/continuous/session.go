package continuous

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

const MaxSessionImportBytes = 4 << 20
const MaxSessionImportRows = 10000

var ErrSessionConflict = errors.New("continuous session ID already exists with different source")

// SessionImport records historical provenance, not proof of task completion.
// Original rows live in immutable entries. Export uses their paired projection.
type SessionImport struct {
	Meta                 core.SessionMeta `json:"meta"`
	Digest               string           `json:"digest"`
	Rows                 int              `json:"rows"`
	ExecutionCheckpoints string           `json:"execution_checkpoints"`
	Repairs              []SessionRepair  `json:"repairs,omitempty"`
}

type SessionRepair struct {
	Row     int      `json:"row"`
	CallIDs []string `json:"call_ids"`
}

type sessionRow struct {
	raw       json.RawMessage
	projected []json.RawMessage
	kind      string
}

// ImportSession accepts a read-only legacy JSONL stream. All entries, ancestry,
// usage rows, model revisions and compaction rows commit in one transaction.
// Identical input retries attach to the original import. Source IDs are retained
// when present, conflicting existing IDs are rejected, never overwritten.
// Historical execution checkpoints are explicitly unavailable. Missing results
// get error-only projection records. No tool intent is created or replayed.
func (r *Runtime) ImportSession(ctx context.Context, source io.Reader) (Conversation, error) {
	data, err := readSessionBytes(ctx, source)
	if err != nil {
		return Conversation{}, err
	}
	rows, meta, repairs, err := parseSessionRows(ctx, data)
	if err != nil {
		return Conversation{}, err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	key := "import/session/" + digest
	id := meta.ID
	if id == "" {
		id = uuid.NewString()
	}
	if len(id) > 128 || strings.ContainsAny(id, "/\\\x00\r\n") || strings.TrimSpace(id) != id {
		return Conversation{}, fmt.Errorf("invalid legacy session ID")
	}
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Conversation{}, err
		}
		existing, ok, err := read[string](snap, key)
		if err != nil {
			return Conversation{}, err
		}
		if ok {
			return conversation(snap, existing)
		}
		if _, ok := snap.Get("conversation/" + id); ok {
			return Conversation{}, ErrSessionConflict
		}
		created := meta.Started
		if created.IsZero() {
			created = time.Now().UTC()
		}
		c := Conversation{ID: id, Created: created, Revision: snap.Revision() + 1, EntrySequence: uint64(len(rows)), Config: AgentConfig{Provider: meta.Provider, Model: meta.Model}}
		info := SessionImport{Meta: meta, Digest: digest, Rows: len(rows), ExecutionCheckpoints: "unavailable", Repairs: repairs}
		ops := []storage.Operation{record("conversation/"+id, c), record(key, id), record("legacy/session/"+id, info)}
		for i, row := range rows {
			e := Entry{ID: uuid.NewString(), ConversationID: id, Revision: c.Revision, Type: "legacy_" + row.kind, LegacyRow: row.raw, SessionProjection: row.projected}
			if meta.ID == "" && row.kind == "meta" {
				var obj map[string]json.RawMessage
				_ = json.Unmarshal(row.raw, &obj)
				idJSON, _ := json.Marshal(id)
				e.SessionProjection = []json.RawMessage{replaceJSONField(row.raw, "meta", replaceJSONField(obj["meta"], "id", idJSON))}
			}
			ops = append(ops, record(entryKey(id, uint64(i+1)), e))
		}
		err = r.commit(ctx, snap, "session.import", ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return c, err
	}
}

func readSessionBytes(ctx context.Context, source io.Reader) ([]byte, error) {
	if source == nil {
		return nil, fmt.Errorf("session reader required")
	}
	var out bytes.Buffer
	emptyReads := 0
	buf := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := source.Read(buf)
		if n > 0 {
			if out.Len()+n > MaxSessionImportBytes {
				return nil, fmt.Errorf("session import exceeds %d bytes", MaxSessionImportBytes)
			}
			out.Write(buf[:n])
		}
		if err == io.EOF {
			return out.Bytes(), nil
		}
		if err != nil {
			return nil, fmt.Errorf("read session: %w", err)
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= 100 {
				return nil, io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
	}
}

func parseSessionRows(ctx context.Context, data []byte) ([]sessionRow, core.SessionMeta, []SessionRepair, error) {
	var rows []sessionRow
	var meta core.SessionMeta
	var repairs []SessionRepair
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 4096), MaxSessionImportBytes+1)
	var segment []int
	// Normalize each historical segment independently. A compaction replaces
	// provider context but never deletes the source segment from history.
	flush := func() error {
		messages := make([]json.RawMessage, len(segment))
		for i, idx := range segment {
			var row struct {
				Message json.RawMessage `json:"message"`
			}
			if err := json.Unmarshal(rows[idx].raw, &row); err != nil {
				return err
			}
			messages[i] = row.Message
		}
		projected, notices, err := pairSessionMessages(messages)
		if err != nil {
			return err
		}
		for i, idx := range segment {
			if projected[i] == nil {
				continue
			}
			for j, msg := range projected[i] {
				if j == 0 {
					rows[idx].projected = append(rows[idx].projected, replaceJSONField(rows[idx].raw, "message", msg))
				} else {
					rows[idx].projected = append(rows[idx].projected, record("", struct {
						Type    string          `json:"type"`
						Message json.RawMessage `json:"message"`
					}{"message", msg}).Value)
				}
			}
		}
		for _, notice := range notices {
			repairs = append(repairs, SessionRepair{Row: segment[notice.Row] + 1, CallIDs: notice.CallIDs})
		}
		segment = nil
		return nil
	}
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, meta, nil, err
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if len(rows) >= MaxSessionImportRows {
			return nil, meta, nil, fmt.Errorf("session import exceeds %d rows", MaxSessionImportRows)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(line, &obj); err != nil || obj == nil {
			return nil, meta, nil, fmt.Errorf("invalid session JSON at row %d", len(rows)+1)
		}
		var kind string
		if err := json.Unmarshal(obj["type"], &kind); err != nil || kind == "" {
			return nil, meta, nil, fmt.Errorf("session row %d has no type", len(rows)+1)
		}
		if len(rows) == 0 && kind != "meta" {
			return nil, meta, nil, fmt.Errorf("session must begin with a meta row")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, line); err != nil {
			return nil, meta, nil, err
		}
		rows = append(rows, sessionRow{raw: bytes.Clone(compact.Bytes()), kind: kind})
		idx := len(rows) - 1
		switch kind {
		case "meta":
			var next *core.SessionMeta
			if err := json.Unmarshal(obj["meta"], &next); err != nil || next == nil {
				return nil, meta, nil, fmt.Errorf("invalid session meta at row %d", idx+1)
			}
			if idx > 0 && next.ID != meta.ID {
				return nil, meta, nil, fmt.Errorf("session ID changes at row %d", idx+1)
			}
			meta = *next
		case "message":
			segment = append(segment, idx)
		case "compaction":
			if err := flush(); err != nil {
				return nil, meta, nil, fmt.Errorf("invalid session transcript before row %d: %w", idx+1, err)
			}
			var messages []json.RawMessage
			if raw, ok := obj["messages"]; ok {
				if err := json.Unmarshal(raw, &messages); err != nil {
					return nil, meta, nil, fmt.Errorf("invalid compaction at row %d", idx+1)
				}
			}
			projected, notices, err := pairSessionMessages(messages)
			if err != nil {
				return nil, meta, nil, fmt.Errorf("invalid compaction transcript at row %d: %w", idx+1, err)
			}
			if len(notices) > 0 {
				var paired []json.RawMessage
				for i, msg := range messages {
					if projected[i] == nil {
						paired = append(paired, msg)
					} else {
						paired = append(paired, projected[i]...)
					}
				}
				b, _ := json.Marshal(paired)
				rows[idx].projected = []json.RawMessage{replaceJSONField(rows[idx].raw, "messages", b)}
				for _, notice := range notices {
					repairs = append(repairs, SessionRepair{Row: idx + 1, CallIDs: notice.CallIDs})
				}
			}
		case "usage":
			for _, field := range []string{"usage", "cumulative"} {
				if raw, ok := obj[field]; ok {
					var u provider.Usage
					if err := json.Unmarshal(raw, &u); err != nil {
						return nil, meta, nil, fmt.Errorf("invalid usage at row %d", idx+1)
					}
				}
			}
			// Unknown typed rows are retained as application records, never executed.
		}
	}
	if err := sc.Err(); err != nil {
		return nil, meta, nil, err
	}
	if len(rows) == 0 {
		return nil, meta, nil, fmt.Errorf("session is empty")
	}
	if err := flush(); err != nil {
		return nil, meta, nil, fmt.Errorf("invalid session transcript: %w", err)
	}
	return rows, meta, repairs, nil
}

func replaceJSONField(raw json.RawMessage, field string, value json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	obj[field] = value
	b, _ := json.Marshal(obj)
	return b
}

func entryKey(id string, sequence uint64) string { return fmt.Sprintf("entry/%s/%020d", id, sequence) }

// ExportSession writes a legacy-compatible projection at one committed snapshot.
// Recovery error stubs are included, original rows remain unchanged in the store.
// Task, approval and document state cannot be represented by this format.
func (r *Runtime) ExportSession(ctx context.Context, id string, out io.Writer) error {
	if out == nil {
		return fmt.Errorf("session writer required")
	}
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return err
	}
	c, err := conversation(snap, id)
	if err != nil {
		return err
	}
	info, imported, err := read[SessionImport](snap, "legacy/session/"+id)
	if err != nil {
		return err
	}
	// New conversations need a legacy header. Imported ones retain every meta
	// revision in transcript order, preserving model switches and ancestry.
	if !imported {
		meta := core.SessionMeta{ID: c.ID, Started: c.Created, Provider: c.Config.Provider, Model: c.Config.Model, Version: "continuous-projection-v1"}
		if err := writeSessionRow(out, record("", struct {
			Type string           `json:"type"`
			Meta core.SessionMeta `json:"meta"`
		}{"meta", meta}).Value); err != nil {
			return err
		}
	}
	// Legacy compaction rows replace everything before them, so a compaction
	// entry is written as the summary plus the verbatim tail that follows its
	// head. The tail rows written earlier remain in the file as history.
	var ownRows []provider.Message
	var ownSeqs []uint64
	steered := map[string]bool{}
	if err := History(ctx, snap, id, 0, func(owner string, seq uint64, e Entry) error {
		if e.Type == entrySteer {
			steered[e.SubmissionID] = true
		}
		if e.Type == "user" && e.SubmissionID != "" {
			if sub, ok, _ := read[Submission](snap, "submission/"+e.SubmissionID); ok && sub.State == "withdrawn" {
				// Withdrawn inputs were never sent; the projection omits them.
				steered[e.SubmissionID] = true
			}
		}
		return nil
	}); err != nil {
		return err
	}
	writeRow := func(e Entry) error {
		switch {
		case e.Type == entryCompaction:
			var info CompactionInfo
			if err := json.Unmarshal(e.Data, &info); err != nil {
				return storage.ErrCorrupt
			}
			summary, err := core.DecodeMessage(e.Message)
			if err != nil {
				return storage.ErrCorrupt
			}
			kept := []provider.Message{summary}
			for i, seq := range ownSeqs {
				if seq >= info.Head {
					kept = append(kept, ownRows[i])
				}
			}
			return writeSessionRow(out, record("", struct {
				Type     string             `json:"type"`
				Messages []provider.Message `json:"messages"`
			}{"compaction", provider.RepairOrphanedToolResults(kept)}).Value)
		case e.Type == entryAttempt, e.Type == entryContext:
			// Failed attempts and context records are not model context
			// and have no legacy row.
			return nil
		case e.Type == entryReset:
			// Legacy files model a context boundary as a compaction with the
			// handoff as its only kept message. History before it is already
			// written above, so readers of the file keep the full transcript.
			var kept []provider.Message
			if e.Content != "" {
				kept = append(kept, provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: e.Content}}})
			}
			if kept == nil {
				kept = []provider.Message{}
			}
			return writeSessionRow(out, record("", struct {
				Type     string             `json:"type"`
				Messages []provider.Message `json:"messages"`
			}{"compaction", kept}).Value)
		case len(e.Message) > 0:
			return writeSessionRow(out, record("", struct {
				Type    string          `json:"type"`
				Message json.RawMessage `json:"message"`
			}{"message", e.Message}).Value)
		case len(e.SessionProjection) > 0:
			for _, row := range e.SessionProjection {
				if err := writeSessionRow(out, row); err != nil {
					return err
				}
			}
			return nil
		case len(e.LegacyRow) > 0:
			return writeSessionRow(out, e.LegacyRow)
		case e.Type == "user" && steered[e.SubmissionID]:
			// A steered input is exported where it joined the run.
			return nil
		case e.Type == "user" || e.Type == entrySteer || e.Type == entryContinue:
			msg := userEntryMessage(e)
			b := marshalMessage(msg)
			return writeSessionRow(out, record("", struct {
				Type    string          `json:"type"`
				Message json.RawMessage `json:"message"`
			}{"message", b}).Value)
		default:
			return fmt.Errorf("unsupported session projection entry type")
		}
	}
	// Forks export their inherited history too, so the file stands alone.
	if err := History(ctx, snap, id, 0, func(owner string, seq uint64, e Entry) error {
		if owner != id && e.Type == "legacy_meta" {
			// An ancestor's legacy header would claim another session ID.
			return nil
		}
		if owner == id {
			switch {
			case len(e.Message) > 0 && e.Type != entryAttempt && e.Type != entryCompaction:
				if msg, err := core.DecodeMessage(e.Message); err == nil {
					ownRows, ownSeqs = append(ownRows, msg), append(ownSeqs, seq)
				}
			case (e.Type == "user" && !steered[e.SubmissionID]) || e.Type == entrySteer:
				ownRows = append(ownRows, userEntryMessage(e))
				ownSeqs = append(ownSeqs, seq)
			}
		}
		return writeRow(e)
	}); err != nil {
		return err
	}
	if imported && (info.Meta.Provider != c.Config.Provider || info.Meta.Model != c.Config.Model) {
		meta := info.Meta
		meta.ID = c.ID
		meta.Provider, meta.Model = c.Config.Provider, c.Config.Model
		return writeSessionRow(out, record("", struct {
			Type string           `json:"type"`
			Meta core.SessionMeta `json:"meta"`
		}{"meta", meta}).Value)
	}
	return nil
}
func writeSessionRow(out io.Writer, row json.RawMessage) error {
	if !json.Valid(row) {
		return storage.ErrCorrupt
	}
	b := append(bytes.Clone(row), '\n')
	n, err := out.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}
