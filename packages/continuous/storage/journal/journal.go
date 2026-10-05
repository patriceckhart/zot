// Package journal provides the default local continuous store: an
// append-only, checksummed commit log with derived, rebuildable indexes.
//
// Authority is the committed prefix of the log, bounded by the boundary
// marker. Everything else under the store directory is derived and
// rebuildable from that prefix: the offset index (revision to frame offset),
// checkpoints (the record index at a revision), and nothing else. Current
// records are tracked by an in-memory persistent tree of key to value offset,
// so memory grows with the number of live keys, not with history or value
// size. Values are read positionally when requested. Open loads the newest
// checkpoint and replays only later frames, so recovery reads a bounded
// suffix. The log is stored in segments so sealed files are never rewritten.
package journal

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/internal/state"
)

const headerSize = 24

var table = crc32.MakeTable(crc32.Castagnoli)
var magic = [4]byte{'Z', 'C', 'J', '1'}

type Options struct {
	Durability storage.Durability
	// SegmentBytes is the size above which the active segment is sealed and
	// a new one started. Zero means DefaultSegmentBytes.
	SegmentBytes int64
	// CheckpointEvery writes a checkpoint of the record index after this
	// many commits. Zero means 1000. Checkpoints bound recovery time; they
	// are derived files and never authoritative.
	CheckpointEvery int
}

// Open acquires an exclusive OS lock for the lifetime of the store. The path
// must be a dedicated local directory. Strict mode is the default. Network
// filesystems and concurrent modification outside this API are unsupported.
func Open(ctx context.Context, path string, opts Options) (storage.Store, error) {
	return open(ctx, path, opts, nil)
}

// Store is the journal-backed continuous store.
type Store struct {
	mu       sync.Mutex
	path     string
	opts     Options
	hook     faultHook
	lock     *os.File
	segs     *segmentSet
	offsets  *offsetIndex
	root     *treap
	epoch    uint64
	revision uint64
	end      int64
	// sinceCheckpoint counts commits since the last checkpoint.
	sinceCheckpoint int
	// staleCheckpoints are unusable checkpoint files found at open, removed
	// once validation succeeds.
	staleCheckpoints []string
	changed          chan struct{}
	closed           bool
}

func open(ctx context.Context, path string, opts Options, hook faultHook) (storage.Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Durability == "" {
		opts.Durability = storage.Strict
	}
	if opts.Durability != storage.Strict && opts.Durability != storage.Process {
		return nil, fmt.Errorf("unsupported journal durability")
	}
	if opts.Durability == storage.Strict && !strictSupported {
		return nil, fmt.Errorf("strict journal durability unavailable on this platform")
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = DefaultSegmentBytes
	}
	if opts.CheckpointEvery <= 0 {
		opts.CheckpointEvery = 1000
	}
	// Require an existing parent so synchronizing it covers creation of this
	// directory, rather than claiming durability for an unsynchronized chain.
	if err := hook.step("open.directory.create", func() error {
		err := os.Mkdir(path, 0o700)
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}); err != nil {
		return nil, fmt.Errorf("create store directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("store must be a real directory")
	}
	lockPath := filepath.Join(path, "writer.lock")
	if _, err := regularFileExists(lockPath); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		return nil, err
	}
	s := &Store{path: path, opts: opts, hook: hook, lock: lock, changed: make(chan struct{})}
	success := false
	defer func() {
		if !success {
			s.closeFiles()
		}
	}()
	dataPath := filepath.Join(path, "commits.log")
	boundaryPath := filepath.Join(path, "boundary")
	dataExists, err := regularFileExists(dataPath)
	if err != nil {
		return nil, err
	}
	boundaryExists, err := regularFileExists(boundaryPath)
	if err != nil {
		return nil, err
	}
	if dataExists != boundaryExists {
		return nil, fmt.Errorf("%w: missing journal file", storage.ErrCorrupt)
	}
	if !dataExists {
		var data *os.File
		if err := hook.step("open.data.create", func() error {
			var err error
			data, err = os.OpenFile(dataPath, os.O_CREATE|os.O_RDWR, 0o600)
			return err
		}); err != nil {
			return nil, err
		}
		if opts.Durability == storage.Strict {
			if err := hook.step("initialize.data.sync", func() error { return durableSync(data) }); err != nil {
				data.Close()
				return nil, err
			}
		}
		data.Close()
		if err := s.publishBoundary("initialize", 0, 0, nil); err != nil {
			return nil, err
		}
	}
	if opts.Durability == storage.Strict {
		if err := hook.step("open.directory.sync", func() error { return syncDirectory(path) }); err != nil {
			return nil, err
		}
		if err := hook.step("open.parent.sync", func() error { return syncDirectory(filepath.Dir(path)) }); err != nil {
			return nil, err
		}
	}
	marker, err := readBoundary(boundaryPath)
	if err != nil {
		return nil, err
	}
	if s.segs, err = openSegments(path, false); err != nil {
		return nil, err
	}
	if len(marker) != 20 || crc32.Checksum(marker[:16], table) != binary.BigEndian.Uint32(marker[16:20]) {
		return nil, storage.ErrCorrupt
	}
	s.revision = binary.BigEndian.Uint64(marker[:8])
	rawEnd := binary.BigEndian.Uint64(marker[8:16])
	if rawEnd > uint64(^uint64(0)>>1) {
		return nil, storage.ErrCorrupt
	}
	s.end = int64(rawEnd)
	if err := s.segs.validate(s.end); err != nil {
		return nil, err
	}
	if s.offsets, err = openOffsetIndex(filepath.Join(path, "indexes")); err != nil {
		return nil, err
	}
	// Recovery reads a bounded suffix: the offset index is reconciled from
	// its last entry (frame headers only), the record index is restored from
	// the newest checkpoint, and only frames after the checkpoint are
	// decoded. Frames before it were validated when they were committed and
	// when the checkpoint was written. Without a usable checkpoint every
	// frame is validated, which is the cold path after a restore.
	if err := s.offsets.reconcile(s.segs, s.revision, s.end); err != nil {
		return nil, err
	}
	if err := s.buildIndex(ctx); err != nil {
		return nil, err
	}
	// The epoch is the last commit's; the acquisition below advances it.
	if s.revision > 0 {
		off, ok, err := s.offsets.offset(s.revision)
		if err != nil || !ok {
			return nil, fmt.Errorf("%w: offset index incomplete", storage.ErrCorrupt)
		}
		last, _, next, err := readFrame(s.segs, off, s.end)
		if err != nil {
			return nil, err
		}
		if next != s.end || last.Epoch == 0 {
			return nil, fmt.Errorf("%w: last frame does not end at the boundary", storage.ErrCorrupt)
		}
		s.epoch = last.Epoch
	}
	// Validation passed: only now are files changed. Derived files are
	// brought up to date and the unacknowledged tail dropped.
	if err := hook.step("recover.data.truncate", func() error { return s.segs.truncate(s.end) }); err != nil {
		return nil, err
	}
	if opts.Durability == storage.Strict {
		if err := hook.step("recover.data.sync", func() error { return durableSync(s.segs.active()) }); err != nil {
			return nil, err
		}
	}
	if err := s.offsets.flush(); err != nil {
		return nil, err
	}
	for _, stale := range s.staleCheckpoints {
		os.Remove(stale)
	}
	s.staleCheckpoints = nil
	if s.revision == ^uint64(0) || s.epoch == ^uint64(0) {
		return nil, fmt.Errorf("store counters exhausted")
	}
	// Acquisition is a commit: the new epoch is durable before any work.
	s.epoch++
	acquire := storage.Commit{Schema: 1, Revision: s.revision + 1, Epoch: s.epoch, Time: time.Now().UTC(), Actor: "writer.acquire"}
	if err := s.persist(acquire); err != nil {
		return nil, err
	}
	success = true
	return s, nil
}

// buildIndex loads the newest checkpoint and applies the commits after it,
// validating each with the transaction rules against the preceding epoch.
func (s *Store) buildIndex(ctx context.Context) error {
	dir := filepath.Join(s.path, "checkpoints")
	root, at, from, stale, err := loadCheckpoint(dir, s.offsets, s.revision, s.end)
	if err != nil {
		return err
	}
	s.root = root
	s.staleCheckpoints = stale
	var epoch uint64
	if at > 0 {
		off, ok, err := s.offsets.offset(at)
		if err != nil || !ok {
			return fmt.Errorf("%w: offset index incomplete", storage.ErrCorrupt)
		}
		c, _, _, err := readFrame(s.segs, off, s.end)
		if err != nil {
			return err
		}
		epoch = c.Epoch
	}
	for r := at + 1; r <= s.revision; r++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, payload, next, err := readFrame(s.segs, from, s.end)
		if err != nil {
			return err
		}
		if err := state.ValidateCommit(c, r, epoch); err != nil {
			return err
		}
		epoch = c.Epoch
		s.applyIndexed(c, from, payload)
		from = next
	}
	if from != s.end {
		return fmt.Errorf("%w: frames do not end at the boundary", storage.ErrCorrupt)
	}
	s.sinceCheckpoint = int(s.revision - at)
	return nil
}

// applyIndexed updates the record index for a commit whose frame starts at
// off with the given payload bytes. The encoder writes each operation value
// in order as compact JSON, so a value's position is found by searching the
// payload forward for its encoded form. A match at an earlier position with
// identical bytes (for example inside the key) still reads back as the same
// value, so the reference is correct wherever it lands.
func (s *Store) applyIndexed(c storage.Commit, off int64, payload []byte) {
	base := off + headerSize
	cursor := 0
	for _, op := range c.Operations {
		if op.Delete {
			s.root = s.root.remove(op.Key)
			continue
		}
		encoded, err := json.Marshal(op.Value)
		if err != nil {
			encoded = op.Value
		}
		i := bytes.Index(payload[cursor:], encoded)
		if i < 0 {
			// Not reachable for a payload produced from c; a zero-length
			// reference reads as corrupt rather than returning wrong bytes.
			s.root = s.root.put(op.Key, recordRef{offset: base, length: 0})
			continue
		}
		i += cursor
		s.root = s.root.put(op.Key, recordRef{offset: base + int64(i), length: int32(len(encoded))})
		cursor = i + len(encoded)
	}
}

func (s *Store) closeFiles() error {
	var errs []error
	if s.offsets != nil {
		errs = append(errs, s.offsets.close())
		s.offsets = nil
	}
	if s.segs != nil {
		errs = append(errs, s.segs.close())
		s.segs = nil
	}
	if s.lock != nil {
		errs = append(errs, unlockFile(s.lock), s.lock.Close())
		s.lock = nil
	}
	return errors.Join(errs...)
}

// publishBoundary atomically replaces the boundary marker. Temporary files
// never control recovery.
func (s *Store) publishBoundary(phase string, revision uint64, end int64, data *os.File) error {
	hook := s.hook
	marker := boundaryMarker(revision, end)
	var f *os.File
	err := hook.step(phase+".boundary.create", func() error {
		var err error
		f, err = os.CreateTemp(s.path, ".boundary-*")
		return err
	})
	if f != nil {
		defer os.Remove(f.Name())
		defer f.Close()
	}
	if err != nil {
		return err
	}
	if err := hook.step(phase+".boundary.write", func() error { return writeAll(f, marker) }); err != nil {
		return err
	}
	if s.opts.Durability == storage.Strict {
		if err := hook.step(phase+".boundary.sync", func() error { return durableSync(f) }); err != nil {
			return err
		}
	}
	if err := hook.step(phase+".boundary.close", f.Close); err != nil {
		return err
	}
	if err := hook.step(phase+".boundary.rename", func() error { return os.Rename(f.Name(), filepath.Join(s.path, "boundary")) }); err != nil {
		return err
	}
	if s.opts.Durability == storage.Strict {
		if err := hook.step(phase+".directory.sync", func() error { return syncDirectory(s.path) }); err != nil {
			return err
		}
		if data != nil {
			// Flush device caches after synchronizing replacement metadata as well.
			return hook.step(phase+".device.sync", func() error { return durableSync(data) })
		}
	}
	return nil
}

// persist appends one frame, publishes the boundary, and updates the
// derived indexes. The boundary rename is the acknowledgement point.
func (s *Store) persist(c storage.Commit) error {
	hook := s.hook
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	h := make([]byte, headerSize)
	copy(h, magic[:])
	binary.BigEndian.PutUint32(h[4:8], uint32(len(b)))
	binary.BigEndian.PutUint64(h[8:16], c.Revision)
	binary.BigEndian.PutUint32(h[16:20], crc32.Checksum(b, table))
	binary.BigEndian.PutUint32(h[20:24], crc32.Checksum(h[:20], table))
	// Rotate before a frame that would push the active segment past the
	// threshold, so a frame never straddles files.
	if s.segs.activeSize() > 0 && s.segs.activeSize()+int64(len(h)+len(b)) > s.opts.SegmentBytes {
		if err := hook.step("commit.segment.rotate", s.segs.rotate); err != nil {
			return err
		}
		if s.opts.Durability == storage.Strict {
			if err := hook.step("commit.segment.sync", func() error { return syncDirectory(s.segs.segmentsDir()) }); err != nil {
				return err
			}
		}
	}
	frameStart := s.end
	if err := hook.step("commit.header.write", func() error { return s.segs.append(h) }); err != nil {
		return err
	}
	if err := hook.step("commit.payload.write", func() error { return s.segs.append(b) }); err != nil {
		return err
	}
	if s.opts.Durability == storage.Strict {
		if err := hook.step("commit.data.sync", func() error { return durableSync(s.segs.active()) }); err != nil {
			return err
		}
	}
	s.end += int64(len(h) + len(b))
	// Persist the high-water mark before acknowledgement. A truncated
	// acknowledged frame or boundary must never look like an uncommitted tail.
	if err := s.publishBoundary("commit", c.Revision, s.end, s.segs.active()); err != nil {
		return err
	}
	s.revision = c.Revision
	// Derived indexes after acknowledgement: a failure here is not a failed
	// commit. The offset index is reconciled at open, the checkpoint is
	// optional.
	s.offsets.append(frameStart)
	_ = s.offsets.flush()
	s.applyIndexed(c, frameStart, b)
	s.sinceCheckpoint++
	if s.sinceCheckpoint >= s.opts.CheckpointEvery {
		s.checkpoint()
	}
	return nil
}

// checkpoint writes the record index at the current revision. Errors are
// ignored: the next open rebuilds from the journal.
func (s *Store) checkpoint() {
	dir := filepath.Join(s.path, "checkpoints")
	if err := writeCheckpoint(dir, s.revision, s.end, s.root, s.opts.Durability == storage.Strict); err != nil {
		return
	}
	_ = s.offsets.sync()
	s.sinceCheckpoint = 0
	pruneCheckpoints(dir, 2)
}

func (s *Store) Capabilities() storage.Capabilities {
	return storage.Capabilities{Durability: s.opts.Durability, Indexed: true, BoundedHistoryMemory: true}
}

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
	if b, err := json.Marshal(c); err != nil {
		return storage.Commit{}, err
	} else if len(b) > state.MaxCommitBytes {
		return storage.Commit{}, fmt.Errorf("commit exceeds %d bytes", state.MaxCommitBytes)
	}
	if err := s.persist(c); err != nil {
		// A failed barrier has an uncertain outcome. Poison the writer rather
		// than allowing a subsequent append to make an invalid tail authoritative.
		s.closed = true
		close(s.changed)
		return storage.Commit{}, fmt.Errorf("commit persistence failed, reopen required: %w", err)
	}
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

// snapshot is a root pointer: the persistent tree makes it free to take and
// immune to later commits. Values are read from the log on demand; a closed
// store makes reads fail rather than return partial state.
type snapshot struct {
	store    *Store
	root     *treap
	revision uint64
	end      int64
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
	return &snapshot{store: s, root: s.root, revision: s.revision, end: s.end}, nil
}

func (v *snapshot) Revision() uint64 { return v.revision }

func (v *snapshot) readValue(ref recordRef) (json.RawMessage, error) {
	if ref.length <= 0 || ref.offset+int64(ref.length) > v.end {
		return nil, fmt.Errorf("%w: record reference outside committed log", storage.ErrCorrupt)
	}
	b := make([]byte, ref.length)
	s := v.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, storage.ErrClosed
	}
	if _, err := s.segs.ReadAt(b, ref.offset); err != nil {
		return nil, err
	}
	return b, nil
}

func (v *snapshot) Get(key string) (json.RawMessage, bool) {
	ref, ok := v.root.get(key)
	if !ok {
		return nil, false
	}
	b, err := v.readValue(ref)
	if err != nil {
		return nil, false
	}
	return b, true
}

func (v *snapshot) Page(prefix, after string, limit int) ([]storage.Record, error) {
	if limit < 1 || limit > state.MaxPage {
		return nil, fmt.Errorf("page limit must be between 1 and %d", state.MaxPage)
	}
	type hit struct {
		key string
		ref recordRef
	}
	var hits []hit
	from := prefix
	if after >= prefix {
		from = after + "\x00"
	}
	v.root.ascend(from, func(key string, ref recordRef) bool {
		if !strings.HasPrefix(key, prefix) {
			return false
		}
		hits = append(hits, hit{key, ref})
		return len(hits) < limit
	})
	out := make([]storage.Record, 0, len(hits))
	for _, h := range hits {
		b, err := v.readValue(h.ref)
		if err != nil {
			return nil, err
		}
		out = append(out, storage.Record{Key: h.key, Value: b})
	}
	return out, nil
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
	if after == s.revision {
		return nil, nil
	}
	off, ok, err := s.offsets.offset(after + 1)
	if err != nil {
		return nil, err
	}
	if !ok {
		// The index should cover every committed revision after open; fall
		// back to a prefix scan rather than failing a read.
		return scanPage(ctx, s.segs, s.end, s.revision, after, limit)
	}
	out := make([]storage.Commit, 0, min(uint64(limit), s.revision-after))
	for r := after + 1; r <= s.revision && len(out) < limit; r++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, _, next, err := readFrame(s.segs, off, s.end)
		if err != nil {
			return nil, err
		}
		if c.Revision != r {
			return nil, fmt.Errorf("%w: scan revision mismatch", storage.ErrCorrupt)
		}
		out = append(out, c)
		off = next
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

// Close writes a final checkpoint so the next open replays nothing, then
// releases the lock.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.changed)
		if s.sinceCheckpoint > 0 && s.offsets != nil {
			s.checkpoint()
		}
	}
	return s.closeFiles()
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("journal file must be regular")
	}
	return true, nil
}

func writeAll(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

func boundaryMarker(revision uint64, end int64) []byte {
	marker := make([]byte, 20)
	binary.BigEndian.PutUint64(marker[:8], revision)
	binary.BigEndian.PutUint64(marker[8:16], uint64(end))
	binary.BigEndian.PutUint32(marker[16:20], crc32.Checksum(marker[:16], table))
	return marker
}

// decodeCommit parses a frame payload.
func decodeCommit(b []byte) (storage.Commit, error) {
	var c storage.Commit
	if err := json.Unmarshal(b, &c); err != nil {
		return storage.Commit{}, storage.ErrCorrupt
	}
	return c, nil
}
