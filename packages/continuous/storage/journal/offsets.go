package journal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// offsetIndex maps revisions to frame offsets in the logical log so a scan
// can start at any revision without reading the prefix. It is a derived
// file: entry r-1 holds the start offset of revision r as 8 bytes plus a 4
// byte checksum. Entries are kept in memory as well (12 bytes per commit)
// and appended to the file after each commit; a file that disagrees with
// the journal is rewritten from the validated in-memory copy. The file is
// never consulted as authority and never written before the journal has
// been validated.
type offsetIndex struct {
	path    string
	f       *os.File
	offsets []int64
	// persisted counts entries already on disk and verified to match.
	persisted uint64
}

const offsetEntrySize = 12

func openOffsetIndex(dir string) (*offsetIndex, error) {
	idx := &offsetIndex{path: filepath.Join(dir, "offsets")}
	b, err := os.ReadFile(idx.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for len(b) >= offsetEntrySize {
		if crc32.Checksum(b[:8], table) != binary.BigEndian.Uint32(b[8:12]) {
			break
		}
		off := binary.BigEndian.Uint64(b[:8])
		if off > uint64(^uint64(0)>>1) {
			break
		}
		idx.offsets = append(idx.offsets, int64(off))
		b = b[offsetEntrySize:]
	}
	idx.persisted = uint64(len(idx.offsets))
	return idx, nil
}

// entries is the number of indexed revisions.
func (idx *offsetIndex) entries() uint64 { return uint64(len(idx.offsets)) }

// offset returns the start of revision r, or ok false when not indexed.
func (idx *offsetIndex) offset(r uint64) (int64, bool, error) {
	if r == 0 || r > idx.entries() {
		return 0, false, nil
	}
	return idx.offsets[r-1], true, nil
}

// truncate drops entries above n.
func (idx *offsetIndex) truncate(n uint64) {
	if n < idx.entries() {
		idx.offsets = idx.offsets[:n]
	}
	if idx.persisted > n {
		idx.persisted = n
	}
}

// append records the offset of the next revision in memory.
func (idx *offsetIndex) append(off int64) { idx.offsets = append(idx.offsets, off) }

// flush writes entries not yet on disk. When the file is longer than the
// verified prefix it is rewritten from the in-memory copy.
func (idx *offsetIndex) flush() error {
	if idx.f == nil {
		if err := os.MkdirAll(filepath.Dir(idx.path), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(idx.path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		idx.f = f
		if err := f.Truncate(int64(idx.persisted * offsetEntrySize)); err != nil {
			return err
		}
	}
	if idx.persisted >= idx.entries() {
		return nil
	}
	buf := make([]byte, 0, (idx.entries()-idx.persisted)*offsetEntrySize)
	var b [offsetEntrySize]byte
	for _, off := range idx.offsets[idx.persisted:] {
		binary.BigEndian.PutUint64(b[:8], uint64(off))
		binary.BigEndian.PutUint32(b[8:12], crc32.Checksum(b[:8], table))
		buf = append(buf, b[:]...)
	}
	if _, err := idx.f.WriteAt(buf, int64(idx.persisted*offsetEntrySize)); err != nil {
		return err
	}
	idx.persisted = idx.entries()
	return nil
}

func (idx *offsetIndex) sync() error {
	if idx.f == nil {
		return nil
	}
	return idx.f.Sync()
}

func (idx *offsetIndex) close() error {
	if idx == nil || idx.f == nil {
		return nil
	}
	err := idx.f.Close()
	idx.f = nil
	return err
}

// reconcile makes the index agree with the committed journal. Every indexed
// entry is checked against the frame header it points to (headers only,
// 24 bytes each, so this is cheap even for long histories) and the chain
// must be contiguous; an index that disagrees anywhere is rebuilt from the
// start. Missing entries are appended by walking frame headers from the
// last valid one. The walk must end exactly at the committed boundary.
func (idx *offsetIndex) reconcile(data io.ReaderAt, revision uint64, end int64) error {
	idx.truncate(revision)
	start := int64(0)
	valid := uint64(0)
	for r := uint64(1); r <= idx.entries(); r++ {
		off, ok, err := idx.offset(r)
		if err != nil || !ok || off != start {
			break
		}
		length, rev, err := readFrameHeader(data, off)
		if err != nil || rev != r || off+headerSize+int64(length) > end {
			break
		}
		start = off + headerSize + int64(length)
		valid = r
	}
	idx.truncate(valid)
	for r := idx.entries() + 1; r <= revision; r++ {
		if start >= end {
			return fmt.Errorf("%w: offset index beyond committed end", storage.ErrCorrupt)
		}
		length, rev, err := readFrameHeader(data, start)
		if err != nil {
			return err
		}
		if rev != r {
			return fmt.Errorf("%w: frame revision %d at offset %d, want %d", storage.ErrCorrupt, rev, start, r)
		}
		idx.append(start)
		start += headerSize + int64(length)
	}
	if revision > 0 && start != end {
		return fmt.Errorf("%w: offset index end %d, boundary %d", storage.ErrCorrupt, start, end)
	}
	return nil
}

// readFrameHeader validates a frame header at off and returns the payload
// length and revision.
func readFrameHeader(data io.ReaderAt, off int64) (uint32, uint64, error) {
	var h [headerSize]byte
	if _, err := data.ReadAt(h[:], off); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, 0, storage.ErrCorrupt
		}
		return 0, 0, err
	}
	if string(h[:4]) != string(magic[:]) || crc32.Checksum(h[:20], table) != binary.BigEndian.Uint32(h[20:24]) {
		return 0, 0, fmt.Errorf("%w: frame header at offset %d", storage.ErrCorrupt, off)
	}
	length := binary.BigEndian.Uint32(h[4:8])
	if length == 0 {
		return 0, 0, storage.ErrCorrupt
	}
	return length, binary.BigEndian.Uint64(h[8:16]), nil
}

// readFrame reads and validates the frame at off, returning the decoded
// commit, its payload bytes, and the offset after it.
func readFrame(data io.ReaderAt, off, end int64) (storage.Commit, []byte, int64, error) {
	length, rev, err := readFrameHeader(data, off)
	if err != nil {
		return storage.Commit{}, nil, 0, err
	}
	next := off + headerSize + int64(length)
	if next > end {
		return storage.Commit{}, nil, 0, fmt.Errorf("%w: frame exceeds committed end", storage.ErrCorrupt)
	}
	var h [headerSize]byte
	if _, err := data.ReadAt(h[:], off); err != nil {
		return storage.Commit{}, nil, 0, err
	}
	b := make([]byte, length)
	if _, err := data.ReadAt(b, off+headerSize); err != nil {
		return storage.Commit{}, nil, 0, storage.ErrCorrupt
	}
	if crc32.Checksum(b, table) != binary.BigEndian.Uint32(h[16:20]) {
		return storage.Commit{}, nil, 0, fmt.Errorf("%w: frame payload checksum", storage.ErrCorrupt)
	}
	c, err := decodeCommit(b)
	if err != nil {
		return storage.Commit{}, nil, 0, err
	}
	if c.Revision != rev {
		return storage.Commit{}, nil, 0, fmt.Errorf("%w: frame revision mismatch", storage.ErrCorrupt)
	}
	return c, b, next, nil
}
