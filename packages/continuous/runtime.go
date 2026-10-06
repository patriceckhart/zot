// Package continuous is the persistent, recoverable execution layer of zot
// continuous: durable admission, committed runs, tasks, documents, forks,
// and a multi-client host. The existing core engine remains the only agent
// execution loop; this package decides what runs and records what happened.
package continuous

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

var ErrRequestConflict = errors.New("continuous request ID reused with different payload")
var ErrNotFound = errors.New("continuous conversation not found")
var ErrQueueFull = errors.New("continuous conversation queue is full")

// AgentConfig is data, not executable code. Permission and environment settings
// will be added with execution integration rather than inferred from a viewer.
type AgentConfig struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Reasoning    string `json:"reasoning,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	// Tools restricts the conversation to the named tools when set. Names
	// not in the host registry are ignored; an empty list means every tool
	// the host offers. DenyTools removes tools regardless of Tools. Both are
	// enforced before authorization and before any approval, so a
	// configuration change never grants a capability the host does not have.
	Tools     []string `json:"tools,omitempty"`
	DenyTools []string `json:"deny_tools,omitempty"`
}

// allows reports whether the configuration permits a tool name.
func (c AgentConfig) allows(name string) bool {
	for _, d := range c.DenyTools {
		if d == name {
			return false
		}
	}
	if len(c.Tools) == 0 {
		return true
	}
	for _, t := range c.Tools {
		if t == name {
			return true
		}
	}
	return false
}

type Conversation struct {
	ID            string      `json:"id"`
	Created       time.Time   `json:"created"`
	Revision      uint64      `json:"revision"`
	Config        AgentConfig `json:"config"`
	QueueSequence uint64      `json:"queue_sequence"`
	EntrySequence uint64      `json:"entry_sequence,omitempty"`
	UsageSequence uint64      `json:"usage_sequence,omitempty"`
	// ConfigRevision counts configuration changes. Decisions bound to a
	// policy (tool-scoped approvals) are valid for one value only.
	ConfigRevision uint64 `json:"config_revision,omitempty"`
	// Parent is set for forks. The child's visible history starts with the
	// parent's entries through Parent.At.
	Parent *Parent `json:"parent,omitempty"`
	// Owner is set for conversations a task created, such as subagents.
	Owner *Owner `json:"owner,omitempty"`
}

type Submission struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	RequestID      string `json:"request_id,omitempty"`
	Actor          string `json:"actor"`
	Content        string `json:"content"`
	Sequence       uint64 `json:"sequence"`
	Revision       uint64 `json:"revision"`
	// State is queued, running, answered, failed, or aborted.
	State string `json:"state"`
	// Policy is queue (default, answered by the next run) or steer (claimed
	// by the active run at its next request boundary).
	Policy string `json:"policy,omitempty"`
}

// SubmitOptions refine admission. The zero value queues behind current work.
type SubmitOptions struct {
	// Policy is "queue" or "steer". Steer inserts the input at the active
	// run's next safe boundary, after the current tool calls settle. Without
	// an active run it behaves like queue.
	Policy string
	// RejectBusy returns ErrBusy instead of admitting when a run is active.
	RejectBusy bool
	// MaxQueue, when positive, rejects admission with ErrQueueFull once the
	// conversation already has that many unclaimed submissions.
	MaxQueue int
}

const (
	PolicyQueue = "queue"
	PolicySteer = "steer"
)

type Entry struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	SubmissionID   string `json:"submission_id"`
	Revision       uint64 `json:"revision"`
	Type           string `json:"type"`
	Content        string `json:"content"`
	// Message is the provider message of assistant, tool_result, and attempt
	// entries, in the session JSON representation decoded by core.DecodeMessage.
	Message json.RawMessage `json:"message,omitempty"`
	// Data carries typed entry data, such as CompactionInfo.
	Data json.RawMessage `json:"data,omitempty"`
	// Time is the commit time of entries written since it was introduced.
	// Older entries have no time and sort by sequence only.
	Time              time.Time         `json:"time,omitzero"`
	LegacyRow         json.RawMessage   `json:"legacy_row,omitempty"`
	SessionProjection []json.RawMessage `json:"session_projection,omitempty"`
}

type Runtime struct {
	store storage.Store
	// claims are the tasks an invocation of this process is running. A
	// task found running in storage but not claimed was interrupted; one
	// that is claimed belongs to a live invocation of another scheduler of
	// this runtime and must not be taken over.
	claims sync.Map
}

// New takes ownership of an already opened store. This initial trusted-local API
// has no authentication or public listener. Actor is audit data, not authorization.
func New(store storage.Store) (*Runtime, error) {
	if store == nil {
		return nil, fmt.Errorf("continuous requires a store")
	}
	snap, err := store.Snapshot(context.Background())
	if err != nil {
		return nil, err
	}
	if f, ok, err := read[RuntimeFormat](snap, runtimeFormatKey); err != nil {
		return nil, err
	} else if ok && f.Version > RuntimeFormatVersion {
		return nil, fmt.Errorf("%w: store uses runtime record format %d, this build supports up to %d", ErrUnsupportedFormat, f.Version, RuntimeFormatVersion)
	}
	return &Runtime{store: store}, nil
}

// RuntimeFormat is the version of the runtime's record layout, separate from
// the backend's commit framing (storage schema). Version 1 is the run-based
// layout; version 2 adds generation chains and task records (chain, progress,
// bgcompaction, task effect and approval waits). A store is raised to 2 by
// the first switch to task execution; older builds would not understand
// those records, so a build refuses stores newer than it supports.
type RuntimeFormat struct {
	Version  int    `json:"version"`
	Revision uint64 `json:"revision"`
}

// RuntimeFormatVersion is the newest runtime record format this build reads.
const RuntimeFormatVersion = 2

const runtimeFormatKey = "runtime/format"

// ErrUnsupportedFormat reports a store written by a newer build.
var ErrUnsupportedFormat = errors.New("continuous store format unsupported")

// formatOps raises the store's runtime format to v when it is lower.
func formatOps(snap storage.Snapshot, v int) []storage.Operation {
	f, ok, _ := read[RuntimeFormat](snap, runtimeFormatKey)
	if ok && f.Version >= v {
		return nil
	}
	return []storage.Operation{record(runtimeFormatKey, RuntimeFormat{Version: v, Revision: snap.Revision() + 1})}
}

// Format reports the store's runtime record format. Absent means 1.
func (r *Runtime) Format(ctx context.Context) (RuntimeFormat, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return RuntimeFormat{}, err
	}
	f, ok, err := read[RuntimeFormat](snap, runtimeFormatKey)
	if !ok && err == nil {
		f.Version = 1
	}
	return f, err
}
func (r *Runtime) Close() error { return r.store.Close() }

// Epoch is the store's current writer epoch, the fencing token remote
// workers use to refuse stale hosts.
func (r *Runtime) Epoch() uint64 { return r.store.Epoch() }
func (r *Runtime) Snapshot(ctx context.Context) (storage.Snapshot, error) {
	return r.store.Snapshot(ctx)
}
func (r *Runtime) Scan(ctx context.Context, after uint64, limit int) ([]storage.Commit, error) {
	return r.store.Scan(ctx, after, limit)
}
func (r *Runtime) Wait(ctx context.Context, after uint64) error { return r.store.Wait(ctx, after) }

func hashedKey(prefix string, parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return prefix + hex.EncodeToString(sum[:])
}
func record(key string, value any) storage.Operation {
	b, _ := json.Marshal(value)
	return storage.Operation{Key: key, Value: b}
}
func read[T any](snap storage.Snapshot, key string) (T, bool, error) {
	var v T
	b, ok := snap.Get(key)
	if !ok {
		return v, false, nil
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, true, fmt.Errorf("%w: invalid continuous record", storage.ErrCorrupt)
	}
	return v, true, nil
}
func (r *Runtime) commit(ctx context.Context, snap storage.Snapshot, actor string, ops ...storage.Operation) error {
	_, err := r.store.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: r.store.Epoch(), Actor: actor, Operations: ops})
	return err
}

// OpenRoot is idempotent within an explicit workspace identity. Later calls do
// not overwrite the original configuration. Identity is not derived from cwd.
func (r *Runtime) OpenRoot(ctx context.Context, workspace string, config AgentConfig) (Conversation, error) {
	if strings.TrimSpace(workspace) == "" {
		return Conversation{}, fmt.Errorf("workspace identity required")
	}
	key := hashedKey("root/", workspace)
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Conversation{}, err
		}
		id, ok, err := read[string](snap, key)
		if err != nil {
			return Conversation{}, err
		}
		if ok {
			return conversation(snap, id)
		}
		c := Conversation{ID: uuid.NewString(), Created: time.Now().UTC(), Revision: snap.Revision() + 1, Config: config}
		err = r.commit(ctx, snap, "root.open", record(key, c.ID), record("conversation/"+c.ID, c))
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return c, err
	}
}
func conversation(snap storage.Snapshot, id string) (Conversation, error) {
	c, ok, err := read[Conversation](snap, "conversation/"+id)
	if err != nil {
		return Conversation{}, err
	}
	if !ok {
		return Conversation{}, ErrNotFound
	}
	return c, nil
}
func (r *Runtime) Conversation(ctx context.Context, id string) (Conversation, error) {
	snap, err := r.store.Snapshot(ctx)
	if err != nil {
		return Conversation{}, err
	}
	return conversation(snap, id)
}

// Configure compares the conversation revision, not the global store revision,
// so unrelated conversations do not cause false client conflicts.
func (r *Runtime) Configure(ctx context.Context, id string, expected uint64, config AgentConfig) (Conversation, error) {
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Conversation{}, err
		}
		c, err := conversation(snap, id)
		if err != nil {
			return Conversation{}, err
		}
		if c.Revision != expected {
			return c, storage.ErrConflict
		}
		c.Config, c.Revision = config, snap.Revision()+1
		c.ConfigRevision++
		err = r.commit(ctx, snap, "conversation.configure", record("conversation/"+id, c))
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return c, err
	}
}

// Submit commits the input, immutable user entry, queue sequence and deduplication
// record atomically. Acceptance is not completion: the submission is answered
// by a later run. Content is preserved byte-for-byte. Request IDs are scoped
// by conversation, actor and operation. Records are retained indefinitely.
func (r *Runtime) Submit(ctx context.Context, id, actor, requestID, content string) (Submission, error) {
	return r.SubmitWith(ctx, id, actor, requestID, content, SubmitOptions{})
}

// SubmitWith is Submit with an explicit scheduling policy. A request-ID retry
// must use the same policy; a different policy is a payload conflict.
func (r *Runtime) SubmitWith(ctx context.Context, id, actor, requestID, content string, opts SubmitOptions) (Submission, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(content) == "" {
		return Submission{}, fmt.Errorf("actor and nonempty content required")
	}
	policy := opts.Policy
	switch policy {
	case "", PolicyQueue:
		policy = ""
	case PolicySteer:
	default:
		return Submission{}, fmt.Errorf("unknown submission policy %q", opts.Policy)
	}
	key := hashedKey("dedup/submit/", id, actor, requestID)
	for {
		snap, err := r.store.Snapshot(ctx)
		if err != nil {
			return Submission{}, err
		}
		c, err := conversation(snap, id)
		if err != nil {
			return Submission{}, err
		}
		if requestID != "" {
			original, ok, err := read[Submission](snap, key)
			if err != nil {
				return Submission{}, err
			}
			if ok {
				if original.Content != content || original.Policy != policy {
					return Submission{}, ErrRequestConflict
				}
				// The deduplication record is the admission. Return the live
				// submission so a retry observes execution progress.
				if current, ok, err := read[Submission](snap, "submission/"+original.ID); err != nil {
					return Submission{}, err
				} else if ok {
					return current, nil
				}
				return original, nil
			}
		}
		held, err := legacyBlocked(snap, id)
		if err != nil {
			return Submission{}, err
		}
		if opts.RejectBusy {
			if run, ok, err := read[Run](snap, runKey(id)); err != nil {
				return Submission{}, err
			} else if ok && run.Phase != "done" {
				return Submission{}, ErrBusy
			}
			if _, ok := snap.Get(chainKey(id)); ok {
				return Submission{}, ErrBusy
			}
		}
		if opts.MaxQueue > 0 {
			queued, err := snap.Page("queue/"+id+"/", "", opts.MaxQueue)
			if err != nil {
				return Submission{}, err
			}
			if len(queued) >= opts.MaxQueue {
				return Submission{}, ErrQueueFull
			}
		}
		s, ops, err := admissionOps(snap, &c, actor, requestID, content, policy)
		if err != nil {
			return Submission{}, err
		}
		if _, active := snap.Get(chainKey(id)); !held && !active {
			// Admission and the generation task that answers it commit
			// together, so an admitted input is never left without its
			// executor after a crash. A conversation still holding an
			// unfinished run of the earlier executor queues instead.
			start, err := startChainOps(pendingSnapshot(snap, ops), c)
			if err != nil {
				return Submission{}, err
			}
			ops = mergeOps(append(ops, start...))
			if len(start) > 0 {
				s.State = "running"
			}
		}
		ops = append(ops, record("conversation/"+id, c))
		err = r.commit(ctx, snap, actor, ops...)
		if errors.Is(err, storage.ErrConflict) {
			continue
		}
		return s, err
	}
}

// admissionOps builds the records of one admission against the conversation
// counters in c, which it advances. The caller commits them together with the
// updated conversation record, possibly alongside other operations.
func admissionOps(snap storage.Snapshot, c *Conversation, actor, requestID, content, policy string) (Submission, []storage.Operation, error) {
	if c.QueueSequence == ^uint64(0) {
		return Submission{}, nil, fmt.Errorf("conversation queue exhausted")
	}
	c.EntrySequence = max(c.EntrySequence, c.QueueSequence)
	if c.EntrySequence == ^uint64(0) {
		return Submission{}, nil, fmt.Errorf("conversation entry sequence exhausted")
	}
	c.QueueSequence++
	c.EntrySequence++
	c.Revision = snap.Revision() + 1
	s := Submission{ID: uuid.NewString(), ConversationID: c.ID, RequestID: requestID, Actor: actor, Content: content, Sequence: c.QueueSequence, Revision: c.Revision, State: "queued", Policy: policy}
	entry := Entry{ID: uuid.NewString(), ConversationID: c.ID, SubmissionID: s.ID, Revision: c.Revision, Type: "user", Content: content, Time: time.Now().UTC()}
	ops := []storage.Operation{
		record("submission/"+s.ID, s),
		record(fmt.Sprintf("queue/%s/%020d", c.ID, s.Sequence), s.ID),
		record(entryKey(c.ID, c.EntrySequence), entry),
	}
	if requestID != "" {
		ops = append(ops, record(hashedKey("dedup/submit/", c.ID, actor, requestID), s))
	}
	return s, ops, nil
}
