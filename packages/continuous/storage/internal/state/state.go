// Package state implements shared transaction semantics for the initial backends.
package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

const MaxPage = 1000
const MaxCommitBytes = 8 << 20

type Store struct {
	mu       sync.Mutex
	epoch    uint64
	revision uint64
	scan     func(context.Context, uint64, int) ([]storage.Commit, error)
	records  map[string]json.RawMessage
	history  []storage.Commit
	changed  chan struct{}
	closed   bool
	caps     storage.Capabilities
	persist  func(storage.Commit) error
	close    func() error
}

// New replays verified commits and installs persistence callbacks. Acquisition
// itself is a commit so a fresh writer epoch survives restart before any work.
func New(caps storage.Capabilities, history []storage.Commit, persist func(storage.Commit) error, closeFn func() error) (*Store, error) {
	return newStore(caps, func(visit func(storage.Commit) error) error {
		for _, c := range history {
			if err := visit(c); err != nil {
				return err
			}
		}
		return nil
	}, nil, persist, closeFn)
}

// NewStreaming replays commits without retaining their payloads. scan reads
// historical commits from the backend. Replay must validate its persistence
// framing before invoking visit. Callbacks run synchronously, and scan, persist
// and close are serialized by the store lock. They must not reenter the store.
// Current records still live in memory, so this is not a bounded-memory backend.
func NewStreaming(caps storage.Capabilities, replay func(func(storage.Commit) error) error, scan func(context.Context, uint64, int) ([]storage.Commit, error), persist func(storage.Commit) error, closeFn func() error) (*Store, error) {
	if replay == nil || scan == nil || persist == nil {
		return nil, fmt.Errorf("streaming store requires replay, scan and persistence")
	}
	return newStore(caps, replay, scan, persist, closeFn)
}

func newStore(caps storage.Capabilities, replay func(func(storage.Commit) error) error, scan func(context.Context, uint64, int) ([]storage.Commit, error), persist func(storage.Commit) error, closeFn func() error) (*Store, error) {
	s := &Store{records: make(map[string]json.RawMessage), changed: make(chan struct{}), caps: caps, persist: persist, close: closeFn, scan: scan}
	if err := replay(func(c storage.Commit) error {
		if s.revision == ^uint64(0) {
			return fmt.Errorf("store revision exhausted")
		}
		if err := ValidateCommit(c, s.revision+1, s.epoch); err != nil {
			return err
		}
		s.apply(c)
		s.epoch = c.Epoch
		return nil
	}); err != nil {
		return nil, err
	}
	if s.revision == ^uint64(0) {
		return nil, fmt.Errorf("store revision exhausted")
	}
	if s.epoch == ^uint64(0) {
		return nil, fmt.Errorf("writer epoch exhausted")
	}
	s.epoch++
	c := storage.Commit{Schema: 1, Revision: s.revision + 1, Epoch: s.epoch, Time: time.Now().UTC(), Actor: "writer.acquire"}
	if persist != nil {
		if err := persist(c); err != nil {
			return nil, err
		}
	}
	s.apply(c)
	return s, nil
}

// ValidateCommit checks persisted transaction semantics without replaying records
// or acquiring a new writer epoch. Journal recovery and inspection share it.
func ValidateCommit(c storage.Commit, revision, previousEpoch uint64) error {
	if c.Schema != 1 || c.Revision != revision || c.Epoch < previousEpoch || c.Epoch == 0 {
		return storage.ErrCorrupt
	}
	if err := validate(c.Operations); err != nil {
		return fmt.Errorf("%w: invalid operations", storage.ErrCorrupt)
	}
	return nil
}

func validate(ops []storage.Operation) error {
	seen := make(map[string]bool)
	for _, op := range ops {
		if op.Key == "" || len(op.Key) > 4096 || seen[op.Key] {
			return fmt.Errorf("invalid or duplicate record key")
		}
		seen[op.Key] = true
		if op.Delete {
			if len(op.Value) != 0 {
				return fmt.Errorf("delete must not contain a value")
			}
		} else if !json.Valid(op.Value) {
			return fmt.Errorf("record value must be valid JSON")
		}
	}
	return nil
}

func cloneCommit(c storage.Commit) storage.Commit {
	c.Operations = append([]storage.Operation(nil), c.Operations...)
	for i := range c.Operations {
		c.Operations[i].Value = bytes.Clone(c.Operations[i].Value)
	}
	return c
}
func (s *Store) apply(c storage.Commit) {
	for _, op := range c.Operations {
		if op.Delete {
			delete(s.records, op.Key)
		} else {
			s.records[op.Key] = bytes.Clone(op.Value)
		}
	}
	s.revision = c.Revision
	if s.scan == nil {
		s.history = append(s.history, cloneCommit(c))
	}
}
func (s *Store) Capabilities() storage.Capabilities { return s.caps }
func (s *Store) Epoch() uint64                      { return s.epoch }

func (s *Store) Commit(ctx context.Context, m storage.Mutation) (storage.Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return storage.Commit{}, err
	}
	if s.closed {
		return storage.Commit{}, storage.ErrClosed
	}
	if m.Epoch != s.epoch {
		return storage.Commit{}, storage.ErrFenced
	}
	if m.ExpectedRevision != s.revision {
		return storage.Commit{}, storage.ErrConflict
	}
	if s.revision == ^uint64(0) {
		return storage.Commit{}, fmt.Errorf("store revision exhausted")
	}
	if err := validate(m.Operations); err != nil {
		return storage.Commit{}, err
	}
	c := cloneCommit(storage.Commit{Schema: 1, Revision: s.revision + 1, Epoch: s.epoch, Time: time.Now().UTC(), Actor: m.Actor, Operations: m.Operations})
	b, err := json.Marshal(c)
	if err != nil {
		return storage.Commit{}, err
	}
	if len(b) > MaxCommitBytes {
		return storage.Commit{}, fmt.Errorf("commit exceeds %d bytes", MaxCommitBytes)
	}
	if s.persist != nil {
		if err := s.persist(c); err != nil {
			// A failed barrier has an uncertain outcome. Poison the writer rather
			// than allowing a subsequent append to make an invalid tail authoritative.
			s.closed = true
			close(s.changed)
			return storage.Commit{}, fmt.Errorf("commit persistence failed, reopen required: %w", err)
		}
	}
	s.apply(c)
	close(s.changed)
	s.changed = make(chan struct{})
	return cloneCommit(c), nil
}

type snapshot struct {
	revision uint64
	records  map[string]json.RawMessage
}

func (v *snapshot) Revision() uint64 { return v.revision }
func (v *snapshot) Get(key string) (json.RawMessage, bool) {
	b, ok := v.records[key]
	return bytes.Clone(b), ok
}
func (v *snapshot) Page(prefix, after string, limit int) ([]storage.Record, error) {
	if limit < 1 || limit > MaxPage {
		return nil, fmt.Errorf("page limit must be between 1 and %d", MaxPage)
	}
	keys := make([]string, 0)
	for k := range v.records {
		if strings.HasPrefix(k, prefix) && k > after {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	out := make([]storage.Record, 0, len(keys))
	for _, k := range keys {
		out = append(out, storage.Record{Key: k, Value: bytes.Clone(v.records[k])})
	}
	return out, nil
}
func (s *Store) Snapshot(ctx context.Context) (storage.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, storage.ErrClosed
	}
	v := &snapshot{revision: s.revision, records: make(map[string]json.RawMessage, len(s.records))}
	// Stored byte slices are immutable. Get and Page copy before returning.
	for k, b := range s.records {
		v.records[k] = b
	}
	return v, nil
}
func (s *Store) Scan(ctx context.Context, after uint64, limit int) ([]storage.Commit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.closed {
		return nil, storage.ErrClosed
	}
	if after > s.revision {
		return nil, storage.ErrCursor
	}
	if limit < 1 || limit > MaxPage {
		return nil, fmt.Errorf("scan limit must be between 1 and %d", MaxPage)
	}
	if s.scan != nil {
		return s.scan(ctx, after, limit)
	}
	n := min(uint64(limit), s.revision-after)
	out := make([]storage.Commit, n)
	for i := range out {
		out[i] = cloneCommit(s.history[after+uint64(i)])
	}
	return out, nil
}
func (s *Store) Wait(ctx context.Context, after uint64) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return storage.ErrClosed
	}
	if after > s.revision {
		s.mu.Unlock()
		return storage.ErrCursor
	}
	if after < s.revision {
		s.mu.Unlock()
		return ctx.Err()
	}
	ch := s.changed
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			return storage.ErrClosed
		}
		return nil
	}
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.changed)
	}
	if s.close == nil {
		return nil
	}
	fn := s.close
	s.close = nil
	return fn()
}
