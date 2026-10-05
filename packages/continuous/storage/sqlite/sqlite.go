// Package sqlite provides a continuous store backed by SQLite through a pure
// Go driver (github.com/ncruces/go-sqlite3, SQLite compiled to WebAssembly
// and run on wazero, which zot already ships). No system SQLite or C
// toolchain is required.
//
// Records are versioned rows: every commit writes one row per operation
// keyed by (key, revision), so a snapshot at revision R reads the newest
// version at or below R without copying anything into memory. Commit
// history is the same table ordered by revision, so Scan never duplicates
// storage. Memory use is bounded by the page size of each read, not by the
// size of the store, and the store opens without loading history.
package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ncruces/go-sqlite3"
	_ "github.com/ncruces/go-sqlite3/embed"
	"github.com/ncruces/go-sqlite3/vfs"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/internal/state"
)

// DatabaseFile is the database file inside a store directory.
const DatabaseFile = "continuous.sqlite"

// CurrentSchema is the table layout version written by this build.
const CurrentSchema = 1

type Options struct {
	// Durability selects the SQLite synchronization level. Strict (default)
	// uses WAL with synchronous=FULL and fullfsync, so every acknowledged
	// commit is flushed through device caches where the platform supports
	// it. Process uses synchronous=NORMAL: acknowledged commits survive a
	// process crash, and the database stays consistent after power loss but
	// may lose the newest commits.
	Durability storage.Durability
}

// Open acquires an exclusive OS lock on <path>/writer.lock for the lifetime
// of the store, creates the database when absent, and acquires a new writer
// epoch. The directory must be local; network filesystems are unsupported.
func Open(ctx context.Context, path string, opts Options) (storage.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Durability == "" {
		opts.Durability = storage.Strict
	}
	if opts.Durability != storage.Strict && opts.Durability != storage.Process {
		return nil, fmt.Errorf("unsupported sqlite durability")
	}
	if !vfs.SupportsFileLocking {
		return nil, fmt.Errorf("sqlite file locking unsupported on this platform")
	}
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("store must be a real directory")
	}
	lock, err := os.OpenFile(filepath.Join(path, "writer.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		return nil, err
	}
	s := &Store{lock: lock, changed: make(chan struct{}), caps: storage.Capabilities{Durability: opts.Durability, Indexed: true, BoundedHistoryMemory: true}}
	success := false
	defer func() {
		if !success {
			s.closeFiles()
		}
	}()
	file := filepath.Join(path, DatabaseFile)
	s.writer, err = openConn(file, opts.Durability, false)
	if err != nil {
		return nil, err
	}
	if err := s.initSchema(); err != nil {
		return nil, err
	}
	if err := s.loadMeta(); err != nil {
		return nil, err
	}
	s.reader, err = openConn(file, opts.Durability, true)
	if err != nil {
		return nil, err
	}
	if s.epoch == ^uint64(0) || s.revision == ^uint64(0) {
		return nil, fmt.Errorf("store counters exhausted")
	}
	// Acquisition is a commit: the new epoch is durable before any work.
	s.epoch++
	acquire := storage.Commit{Schema: 1, Revision: s.revision + 1, Epoch: s.epoch, Time: time.Now().UTC(), Actor: "writer.acquire"}
	if err := s.persist(acquire); err != nil {
		return nil, err
	}
	s.revision = acquire.Revision
	success = true
	return s, nil
}

// openConn opens a connection with the durability pragmas applied. Read-only
// connections still need write access to the WAL index, so they open the
// file normally and rely on the API to never write.
func openConn(file string, durability storage.Durability, reader bool) (*sqlite3.Conn, error) {
	conn, err := sqlite3.OpenFlags(file, sqlite3.OPEN_READWRITE|sqlite3.OPEN_CREATE|sqlite3.OPEN_EXRESCODE)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}
	pragmas := []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"}
	if !reader {
		pragmas = append(pragmas, "PRAGMA journal_mode=WAL")
		if durability == storage.Strict {
			pragmas = append(pragmas, "PRAGMA synchronous=FULL", "PRAGMA fullfsync=ON", "PRAGMA checkpoint_fullfsync=ON")
		} else {
			pragmas = append(pragmas, "PRAGMA synchronous=NORMAL")
		}
	}
	for _, p := range pragmas {
		if err := conn.Exec(p); err != nil {
			conn.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return conn, nil
}

// Store is the SQLite-backed continuous store.
type Store struct {
	mu       sync.Mutex
	writer   *sqlite3.Conn
	readMu   sync.Mutex
	reader   *sqlite3.Conn
	lock     *os.File
	epoch    uint64
	revision uint64
	changed  chan struct{}
	closed   bool
	caps     storage.Capabilities
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS meta (
	name TEXT PRIMARY KEY,
	value INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS commits (
	revision INTEGER PRIMARY KEY,
	epoch INTEGER NOT NULL,
	time TEXT NOT NULL,
	actor TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS records (
	key TEXT NOT NULL,
	revision INTEGER NOT NULL REFERENCES commits(revision),
	ordinal INTEGER NOT NULL,
	deleted INTEGER NOT NULL DEFAULT 0,
	value BLOB,
	PRIMARY KEY (key, revision)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS records_by_revision ON records(revision, ordinal);
`

func (s *Store) initSchema() error {
	if err := s.writer.Exec(schemaSQL); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	stmt, _, err := s.writer.Prepare("SELECT value FROM meta WHERE name = 'schema'")
	if err != nil {
		return err
	}
	defer stmt.Close()
	if stmt.Step() {
		if got := stmt.ColumnInt64(0); got != CurrentSchema {
			return fmt.Errorf("%w: sqlite store schema %d, this build supports %d", storage.ErrCorrupt, got, CurrentSchema)
		}
		return stmt.Err()
	}
	if err := stmt.Err(); err != nil {
		return err
	}
	return s.writer.Exec(fmt.Sprintf("INSERT INTO meta(name, value) VALUES ('schema', %d), ('revision', 0), ('epoch', 0)", CurrentSchema))
}

func (s *Store) loadMeta() error {
	stmt, _, err := s.writer.Prepare("SELECT name, value FROM meta WHERE name IN ('revision', 'epoch')")
	if err != nil {
		return err
	}
	defer stmt.Close()
	seen := 0
	for stmt.Step() {
		v := stmt.ColumnInt64(0 + 1)
		if v < 0 {
			return storage.ErrCorrupt
		}
		switch stmt.ColumnText(0) {
		case "revision":
			s.revision = uint64(v)
		case "epoch":
			s.epoch = uint64(v)
		}
		seen++
	}
	if err := stmt.Err(); err != nil {
		return err
	}
	if seen != 2 {
		return fmt.Errorf("%w: missing store counters", storage.ErrCorrupt)
	}
	// The newest commit row must agree with the counters, otherwise the
	// database was modified outside this API.
	check, _, err := s.writer.Prepare("SELECT coalesce(max(revision), 0), coalesce((SELECT epoch FROM commits ORDER BY revision DESC LIMIT 1), 0) FROM commits")
	if err != nil {
		return err
	}
	defer check.Close()
	if check.Step() {
		if uint64(check.ColumnInt64(0)) != s.revision || uint64(check.ColumnInt64(1)) != s.epoch {
			return fmt.Errorf("%w: commit history disagrees with store counters", storage.ErrCorrupt)
		}
	}
	return check.Err()
}

func (s *Store) closeFiles() error {
	var errs []error
	if s.reader != nil {
		errs = append(errs, s.reader.Close())
		s.reader = nil
	}
	if s.writer != nil {
		errs = append(errs, s.writer.Close())
		s.writer = nil
	}
	if s.lock != nil {
		errs = append(errs, unlockFile(s.lock), s.lock.Close())
		s.lock = nil
	}
	return errors.Join(errs...)
}

// persist writes one validated commit in a single SQLite transaction and
// updates the counters. The transaction's commit is the persistence barrier.
func (s *Store) persist(c storage.Commit) (retErr error) {
	tx, err := s.writer.BeginImmediate()
	if err != nil {
		return err
	}
	defer tx.End(&retErr)
	insert, _, err := s.writer.Prepare("INSERT INTO commits(revision, epoch, time, actor) VALUES (?, ?, ?, ?)")
	if err != nil {
		return err
	}
	defer insert.Close()
	insert.BindInt64(1, int64(c.Revision))
	insert.BindInt64(2, int64(c.Epoch))
	insert.BindText(3, c.Time.UTC().Format(time.RFC3339Nano))
	insert.BindText(4, c.Actor)
	if err := insert.Exec(); err != nil {
		return err
	}
	if len(c.Operations) > 0 {
		row, _, err := s.writer.Prepare("INSERT INTO records(key, revision, ordinal, deleted, value) VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer row.Close()
		for i, op := range c.Operations {
			row.BindText(1, op.Key)
			row.BindInt64(2, int64(c.Revision))
			row.BindInt64(3, int64(i))
			row.BindBool(4, op.Delete)
			if op.Delete {
				row.BindNull(5)
			} else {
				row.BindBlob(5, op.Value)
			}
			if err := row.Exec(); err != nil {
				return err
			}
			if err := row.Reset(); err != nil {
				return err
			}
		}
	}
	update, _, err := s.writer.Prepare("UPDATE meta SET value = CASE name WHEN 'revision' THEN ? WHEN 'epoch' THEN ? END WHERE name IN ('revision', 'epoch')")
	if err != nil {
		return err
	}
	defer update.Close()
	update.BindInt64(1, int64(c.Revision))
	update.BindInt64(2, int64(c.Epoch))
	return update.Exec()
}

func (s *Store) Capabilities() storage.Capabilities { return s.caps }

func (s *Store) Epoch() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

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
	c := storage.Commit{Schema: 1, Revision: s.revision + 1, Epoch: s.epoch, Time: time.Now().UTC(), Actor: m.Actor, Operations: cloneOperations(m.Operations)}
	if err := state.ValidateCommit(c, c.Revision, s.epoch); err != nil {
		return storage.Commit{}, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return storage.Commit{}, err
	}
	if len(b) > state.MaxCommitBytes {
		return storage.Commit{}, fmt.Errorf("commit exceeds %d bytes", state.MaxCommitBytes)
	}
	if err := s.persist(c); err != nil {
		// A failed barrier has an uncertain outcome. Poison the writer rather
		// than let a later commit build on an unknown state.
		s.closed = true
		close(s.changed)
		return storage.Commit{}, fmt.Errorf("commit persistence failed, reopen required: %w", err)
	}
	s.revision = c.Revision
	close(s.changed)
	s.changed = make(chan struct{})
	return storage.Commit{Schema: c.Schema, Revision: c.Revision, Epoch: c.Epoch, Time: c.Time, Actor: c.Actor, Operations: cloneOperations(c.Operations)}, nil
}

func cloneOperations(ops []storage.Operation) []storage.Operation {
	out := make([]storage.Operation, len(ops))
	for i, op := range ops {
		out[i] = storage.Operation{Key: op.Key, Delete: op.Delete, Value: bytes.Clone(op.Value)}
	}
	return out
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
	return &snapshot{store: s, revision: s.revision}, nil
}

// snapshot reads versions at or below its revision. Rows above it are
// invisible, so a later commit never changes what it returns; a closed
// store makes reads fail rather than return partial state.
type snapshot struct {
	store    *Store
	revision uint64
}

func (v *snapshot) Revision() uint64 { return v.revision }

func (v *snapshot) Get(key string) (json.RawMessage, bool) {
	s := v.store
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.reader == nil {
		return nil, false
	}
	stmt, _, err := s.reader.Prepare("SELECT deleted, value FROM records WHERE key = ? AND revision <= ? ORDER BY revision DESC LIMIT 1")
	if err != nil {
		return nil, false
	}
	defer stmt.Close()
	stmt.BindText(1, key)
	stmt.BindInt64(2, int64(v.revision))
	if !stmt.Step() || stmt.ColumnBool(0) {
		return nil, false
	}
	return bytes.Clone(stmt.ColumnRawBlob(1)), true
}

func (v *snapshot) Page(prefix, after string, limit int) ([]storage.Record, error) {
	if limit < 1 || limit > state.MaxPage {
		return nil, fmt.Errorf("page limit must be between 1 and %d", state.MaxPage)
	}
	s := v.store
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.reader == nil {
		return nil, storage.ErrClosed
	}
	// SQLite returns the other columns of the row holding max(revision) for
	// a bare-column aggregate, which gives the newest visible version.
	stmt, _, err := s.reader.Prepare("SELECT key, max(revision), deleted, value FROM records WHERE key > ? AND key < ? AND revision <= ? GROUP BY key ORDER BY key LIMIT ?")
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	// Keys at or above the prefix and below its upper bound match. The
	// cursor is exclusive; starting below the prefix uses a value just under
	// it so the prefix itself is a candidate.
	cursor := after
	if cursor < prefix {
		cursor = prefixBefore(prefix)
	}
	upper := prefixUpperBound(prefix)
	out := make([]storage.Record, 0, limit)
	for len(out) < limit {
		// Deleted versions occupy rows but not results, so fetch in
		// batches until a batch comes back short.
		batch := limit - len(out)
		stmt.BindText(1, cursor)
		stmt.BindText(2, upper)
		stmt.BindInt64(3, int64(v.revision))
		stmt.BindInt64(4, int64(batch))
		rows := 0
		for stmt.Step() {
			rows++
			cursor = stmt.ColumnText(0)
			if stmt.ColumnBool(2) {
				continue
			}
			out = append(out, storage.Record{Key: cursor, Value: bytes.Clone(stmt.ColumnRawBlob(3))})
		}
		if err := stmt.Err(); err != nil {
			return nil, err
		}
		if err := stmt.Reset(); err != nil {
			return nil, err
		}
		if rows < batch {
			break
		}
	}
	return out, nil
}

// prefixBefore returns a string strictly below every key with the prefix
// and at or above every key below it: the prefix with its last nonzero byte
// decremented and padded high. The empty prefix has no lower bound.
func prefixBefore(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] > 0 {
			b[i]--
			return string(b[:i+1]) + strings.Repeat("\xff", 8)
		}
	}
	return ""
}

// prefixUpperBound is the smallest string above every key with the prefix.
// A prefix of all 0xFF bytes (or the empty prefix) has no bound; a high
// sentinel is used instead.
func prefixUpperBound(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1])
		}
	}
	return strings.Repeat("\xff", 16)
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
	if limit < 1 || limit > state.MaxPage {
		return nil, fmt.Errorf("scan limit must be between 1 and %d", state.MaxPage)
	}
	return scanCommits(s.writer, after, limit)
}

// scanCommits reads commits after a revision with their operations in order.
func scanCommits(conn *sqlite3.Conn, after uint64, limit int) ([]storage.Commit, error) {
	heads, _, err := conn.Prepare("SELECT revision, epoch, time, actor FROM commits WHERE revision > ? ORDER BY revision LIMIT ?")
	if err != nil {
		return nil, err
	}
	defer heads.Close()
	heads.BindInt64(1, int64(after))
	heads.BindInt64(2, int64(limit))
	var out []storage.Commit
	for heads.Step() {
		t, err := time.Parse(time.RFC3339Nano, heads.ColumnText(2))
		if err != nil {
			return nil, fmt.Errorf("%w: commit time", storage.ErrCorrupt)
		}
		out = append(out, storage.Commit{Schema: 1, Revision: uint64(heads.ColumnInt64(0)), Epoch: uint64(heads.ColumnInt64(1)), Time: t, Actor: heads.ColumnText(3)})
	}
	if err := heads.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	ops, _, err := conn.Prepare("SELECT key, deleted, value FROM records WHERE revision = ? ORDER BY ordinal")
	if err != nil {
		return nil, err
	}
	defer ops.Close()
	for i := range out {
		ops.BindInt64(1, int64(out[i].Revision))
		for ops.Step() {
			op := storage.Operation{Key: ops.ColumnText(0), Delete: ops.ColumnBool(1)}
			if !op.Delete {
				op.Value = bytes.Clone(ops.ColumnRawBlob(2))
			}
			out[i].Operations = append(out[i].Operations, op)
		}
		if err := ops.Err(); err != nil {
			return nil, err
		}
		if err := ops.Reset(); err != nil {
			return nil, err
		}
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
	s.readMu.Lock()
	defer s.readMu.Unlock()
	return s.closeFiles()
}
