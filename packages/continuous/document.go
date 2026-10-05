package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// DocumentDefinition declares a typed JSON document kind. Documents live in
// the same transactional store as entries and tasks, so a transcript change
// and a document update can commit together.
type DocumentDefinition struct {
	// Kind is stable across versions. Version pins the value schema.
	Kind    string
	Version int
	// Scope is conversation or runtime.
	Scope string
	// Initial returns the value of a document that has never been written.
	Initial func() any
	// Validate rejects a value before it commits. Nil accepts any JSON.
	Validate func(value json.RawMessage) error
	// Migrate converts a value from an older version. Nil rejects older
	// versions, which blocks reads until a migration exists.
	Migrate func(fromVersion int, value json.RawMessage) (json.RawMessage, error)
	// Fork is as_of, current, or fresh. It decides what a forked conversation
	// sees: the parent's value at the fork entry, the parent's current value
	// at fork time, or Initial. as_of requires History true.
	Fork string
	// History retains every committed value so as_of forks and historical
	// reads work. Current-only documents keep the latest value.
	History bool
}

// Document is one committed document value.
type Document struct {
	Kind           string          `json:"kind"`
	Version        int             `json:"version"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Value          json.RawMessage `json:"value"`
	// Revision is the store revision of the commit that wrote this value.
	// Mutations must present it to avoid lost updates.
	Revision uint64 `json:"revision"`
	// Sequence counts writes of this document, used for history keys.
	Sequence uint64 `json:"sequence"`
	// EntrySequence is the conversation's entry sequence at write time, so
	// as_of forks can find the value visible at a fork entry.
	EntrySequence uint64 `json:"entry_sequence,omitempty"`
	Deleted       bool   `json:"deleted,omitempty"`
}

var ErrDocumentConflict = errors.New("continuous document revision conflict")
var ErrDocumentBlocked = errors.New("continuous document blocked: definition missing or incompatible")

func documentKey(kind, conversationID string) string {
	if conversationID == "" {
		return "doc/runtime/" + kind
	}
	return "doc/conversation/" + conversationID + "/" + kind
}

func documentHistoryKey(kind, conversationID string, sequence uint64) string {
	if conversationID == "" {
		return fmt.Sprintf("doc-history/runtime/%s/%020d", kind, sequence)
	}
	return fmt.Sprintf("doc-history/conversation/%s/%s/%020d", conversationID, kind, sequence)
}

// DocumentRegistry maps kinds to definitions.
type DocumentRegistry map[string]DocumentDefinition

func (reg DocumentRegistry) Register(def DocumentDefinition) error {
	if strings.TrimSpace(def.Kind) == "" || def.Version < 1 || def.Initial == nil {
		return fmt.Errorf("document definition requires kind, version, and initial")
	}
	if def.Scope != "conversation" && def.Scope != "runtime" {
		return fmt.Errorf("document scope must be conversation or runtime")
	}
	switch def.Fork {
	case "", "fresh":
		def.Fork = "fresh"
	case "current":
	case "as_of":
		if !def.History {
			return fmt.Errorf("as_of fork policy requires history")
		}
	default:
		return fmt.Errorf("document fork policy must be as_of, current, or fresh")
	}
	if def.Scope == "runtime" && def.Fork != "fresh" {
		return fmt.Errorf("runtime documents are not forked")
	}
	reg[def.Kind] = def
	return nil
}

// ReadDocument returns the committed value, resolving fork inheritance and
// applying migrations in memory. A never-written document returns Initial
// with Revision 0. Reads never commit.
func (r *Runtime) ReadDocument(ctx context.Context, reg DocumentRegistry, kind, conversationID string) (Document, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Document{}, err
	}
	return readDocument(snap, reg, kind, conversationID)
}

func readDocument(snap storage.Snapshot, reg DocumentRegistry, kind, conversationID string) (Document, error) {
	def, ok := reg[kind]
	if !ok {
		return Document{}, fmt.Errorf("%w: unknown kind", ErrDocumentBlocked)
	}
	if (def.Scope == "runtime") != (conversationID == "") {
		return Document{}, fmt.Errorf("document scope mismatch")
	}
	doc, found, err := read[Document](snap, documentKey(kind, conversationID))
	if err != nil {
		return Document{}, err
	}
	if found && !doc.Deleted {
		return migrateDocument(def, doc)
	}
	if found && doc.Deleted {
		return initialDocument(def, conversationID, doc.Revision, doc.Sequence), nil
	}
	// Never written here. A fork may inherit from its parent.
	if conversationID != "" {
		c, err := conversation(snap, conversationID)
		if err != nil {
			return Document{}, err
		}
		if c.Parent != nil && def.Fork != "fresh" {
			inherited, err := inheritedDocument(snap, reg, def, c)
			if err != nil {
				return Document{}, err
			}
			if inherited != nil {
				return *inherited, nil
			}
		}
	}
	return initialDocument(def, conversationID, 0, 0), nil
}

func initialDocument(def DocumentDefinition, conversationID string, revision, sequence uint64) Document {
	value, _ := json.Marshal(def.Initial())
	return Document{Kind: def.Kind, Version: def.Version, ConversationID: conversationID, Value: value, Revision: revision, Sequence: sequence}
}

func migrateDocument(def DocumentDefinition, doc Document) (Document, error) {
	if doc.Version > def.Version {
		return Document{}, fmt.Errorf("%w: stored version %d newer than %d", ErrDocumentBlocked, doc.Version, def.Version)
	}
	if doc.Version < def.Version {
		if def.Migrate == nil {
			return Document{}, fmt.Errorf("%w: no migration from version %d", ErrDocumentBlocked, doc.Version)
		}
		value, err := def.Migrate(doc.Version, doc.Value)
		if err != nil {
			return Document{}, fmt.Errorf("%w: migration failed: %v", ErrDocumentBlocked, err)
		}
		doc.Value, doc.Version = value, def.Version
	}
	return doc, nil
}

// inheritedDocument resolves a fork's view of its parent's document. The
// parent may itself be a fork, so the walk continues until a written value or
// a root is found. Nil means the chain never wrote the document.
func inheritedDocument(snap storage.Snapshot, reg DocumentRegistry, def DocumentDefinition, child Conversation) (*Document, error) {
	parentID, at := child.Parent.ConversationID, child.Parent.At
	seen := map[string]bool{child.ID: true}
	for parentID != "" {
		if seen[parentID] {
			return nil, fmt.Errorf("%w: fork ancestry cycle", storage.ErrCorrupt)
		}
		seen[parentID] = true
		var doc Document
		var found bool
		var err error
		switch def.Fork {
		case "current":
			doc, found, err = read[Document](snap, documentKey(def.Kind, parentID))
		case "as_of":
			doc, found, err = documentAsOf(snap, def.Kind, parentID, at)
		}
		if err != nil {
			return nil, err
		}
		if found {
			if doc.Deleted {
				return nil, nil
			}
			migrated, err := migrateDocument(def, doc)
			if err != nil {
				return nil, err
			}
			// Inherited values carry the child's identity and revision 0 so a
			// child write does not need the parent's revision.
			migrated.ConversationID, migrated.Revision, migrated.Sequence = child.ID, 0, 0
			return &migrated, nil
		}
		parent, err := conversation(snap, parentID)
		if err != nil {
			return nil, err
		}
		if parent.Parent == nil {
			return nil, nil
		}
		parentID, at = parent.Parent.ConversationID, parent.Parent.At
	}
	return nil, nil
}

// documentAsOf finds the newest historical value written at or before the
// given entry sequence of the conversation.
func documentAsOf(snap storage.Snapshot, kind, conversationID string, entrySeq uint64) (Document, bool, error) {
	prefix := fmt.Sprintf("doc-history/conversation/%s/%s/", conversationID, kind)
	after := ""
	var best Document
	found := false
	for {
		page, err := snap.Page(prefix, after, 200)
		if err != nil {
			return Document{}, false, err
		}
		if len(page) == 0 {
			return best, found, nil
		}
		for _, row := range page {
			after = row.Key
			var doc Document
			if err := json.Unmarshal(row.Value, &doc); err != nil {
				return Document{}, false, storage.ErrCorrupt
			}
			if doc.EntrySequence > entrySeq {
				return best, found, nil
			}
			best, found = doc, true
		}
	}
}

// WriteDocument commits a new value. expectedRevision must equal the current
// document revision (0 for a never-written or inherited document) or the
// write conflicts. The write validates, records history when the definition
// keeps it, and advances the conversation revision for conversation scope.
func (r *Runtime) WriteDocument(ctx context.Context, reg DocumentRegistry, kind, conversationID string, expectedRevision uint64, value any) (Document, error) {
	return r.mutateDocument(ctx, reg, kind, conversationID, expectedRevision, value, false)
}

// DeleteDocument records deletion. Reads afterwards return Initial. History
// keeps earlier values.
func (r *Runtime) DeleteDocument(ctx context.Context, reg DocumentRegistry, kind, conversationID string, expectedRevision uint64) (Document, error) {
	return r.mutateDocument(ctx, reg, kind, conversationID, expectedRevision, nil, true)
}

func (r *Runtime) mutateDocument(ctx context.Context, reg DocumentRegistry, kind, conversationID string, expectedRevision uint64, value any, deleted bool) (Document, error) {
	def, ok := reg[kind]
	if !ok {
		return Document{}, fmt.Errorf("%w: unknown kind", ErrDocumentBlocked)
	}
	var raw json.RawMessage
	if !deleted {
		var err error
		raw, err = json.Marshal(value)
		if err != nil {
			return Document{}, err
		}
		if def.Validate != nil {
			if err := def.Validate(raw); err != nil {
				return Document{}, fmt.Errorf("document validation: %w", err)
			}
		}
	}
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Document{}, err
		}
		current, err := readDocument(snap, reg, kind, conversationID)
		if err != nil {
			return Document{}, err
		}
		if current.Revision != expectedRevision {
			return current, ErrDocumentConflict
		}
		ops, doc, err := documentOps(snap, def, conversationID, current, raw, deleted)
		if err != nil {
			return Document{}, err
		}
		err = r.commit(ctx, snap, "document.write", ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return doc, err
	}
}

// documentOps builds the operations for one document write at a snapshot.
// Task phases use it through Next.Ops to commit a document with their
// checkpoint.
func documentOps(snap storage.Snapshot, def DocumentDefinition, conversationID string, current Document, value json.RawMessage, deleted bool) ([]storage.Operation, Document, error) {
	doc := Document{Kind: def.Kind, Version: def.Version, ConversationID: conversationID, Value: value, Revision: snap.Revision() + 1, Sequence: current.Sequence + 1, Deleted: deleted}
	if deleted {
		doc.Value = nil
	}
	ops := []storage.Operation{}
	if conversationID != "" {
		c, err := conversation(snap, conversationID)
		if err != nil {
			return nil, Document{}, err
		}
		doc.EntrySequence = c.EntrySequence
		c.Revision = snap.Revision() + 1
		ops = append(ops, record("conversation/"+conversationID, c))
	}
	ops = append(ops, record(documentKey(def.Kind, conversationID), doc))
	if def.History {
		ops = append(ops, record(documentHistoryKey(def.Kind, conversationID, doc.Sequence), doc))
	}
	return ops, doc, nil
}

// DocumentWrite is a helper for task phases: it resolves the current value at
// the phase's snapshot and returns the operations to commit a new one. The
// revision check uses the snapshot the phase already holds.
func DocumentWrite(snap storage.Snapshot, reg DocumentRegistry, kind, conversationID string, value any) ([]storage.Operation, error) {
	def, ok := reg[kind]
	if !ok {
		return nil, fmt.Errorf("%w: unknown kind", ErrDocumentBlocked)
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if def.Validate != nil {
		if err := def.Validate(raw); err != nil {
			return nil, fmt.Errorf("document validation: %w", err)
		}
	}
	current, err := readDocument(snap, reg, kind, conversationID)
	if err != nil {
		return nil, err
	}
	ops, _, err := documentOps(snap, def, conversationID, current, raw, false)
	return ops, err
}
