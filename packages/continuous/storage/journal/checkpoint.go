package journal

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// A checkpoint is the record index at one committed revision, written as a
// derived file under checkpoints/. Open loads the newest valid checkpoint
// and replays only the commits after it, so recovery reads a bounded
// suffix of the journal rather than every frame. Checkpoints are never
// authoritative: a missing, torn, or stale checkpoint is ignored and the
// index is rebuilt from the journal.
//
// Layout: 4 byte magic, 8 byte revision, 8 byte committed end, then entries
// of (2 byte key length, key, 8 byte offset, 4 byte length), then an 8 byte
// entry count and a 4 byte CRC32 over everything before it.

var checkpointMagic = [4]byte{'Z', 'C', 'K', '1'}

const checkpointHeaderSize = 20

// checkpointName is the file for the checkpoint at a revision.
func checkpointName(dir string, revision uint64) string {
	return filepath.Join(dir, fmt.Sprintf("%020d.ckpt", revision))
}

// writeCheckpoint writes the index of root at revision atomically.
func writeCheckpoint(dir string, revision uint64, end int64, root *treap, sync bool) (retErr error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".ckpt-*")
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	crc := crc32.New(table)
	w := bufio.NewWriterSize(io.MultiWriter(f, crc), 1<<16)
	var header [checkpointHeaderSize]byte
	copy(header[:4], checkpointMagic[:])
	binary.BigEndian.PutUint64(header[4:12], revision)
	binary.BigEndian.PutUint64(header[12:20], uint64(end))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	var count uint64
	var scratch [14]byte
	var werr error
	root.ascend("", func(key string, ref recordRef) bool {
		if len(key) > 0xffff {
			werr = fmt.Errorf("record key too long for checkpoint")
			return false
		}
		binary.BigEndian.PutUint16(scratch[:2], uint16(len(key)))
		if _, werr = w.Write(scratch[:2]); werr != nil {
			return false
		}
		if _, werr = w.WriteString(key); werr != nil {
			return false
		}
		binary.BigEndian.PutUint64(scratch[:8], uint64(ref.offset))
		binary.BigEndian.PutUint32(scratch[8:12], uint32(ref.length))
		if _, werr = w.Write(scratch[:12]); werr != nil {
			return false
		}
		count++
		return true
	})
	if werr != nil {
		return werr
	}
	binary.BigEndian.PutUint64(scratch[:8], count)
	if _, err := w.Write(scratch[:8]); err != nil {
		return err
	}
	if err := w.Flush(); err != nil {
		return err
	}
	binary.BigEndian.PutUint32(scratch[:4], crc.Sum32())
	if _, err := f.Write(scratch[:4]); err != nil {
		return err
	}
	if sync {
		if err := durableSync(f); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), checkpointName(dir, revision)); err != nil {
		return err
	}
	if sync {
		if err := syncDirectory(dir); err != nil && !isDirSyncUnsupported(err) {
			return err
		}
	}
	return nil
}

// isDirSyncUnsupported tolerates platforms where directory synchronization
// is not available; checkpoints are derived data, so a missing directory
// sync only risks losing the checkpoint, never committed state.
func isDirSyncUnsupported(err error) bool {
	return err != nil && !strictSupported
}

// loadCheckpoint reads the newest checkpoint at or below revision whose
// recorded end matches the journal's committed end for that revision as
// verified by the offset index. It returns a nil root when none is usable,
// plus the paths of unusable files for the caller to remove after the
// journal itself has been validated. Nothing is written here.
func loadCheckpoint(dir string, idx *offsetIndex, revision uint64, end int64) (*treap, uint64, int64, []string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, 0, nil, nil
		}
		return nil, 0, 0, nil, err
	}
	var stale []string
	for i := len(entries) - 1; i >= 0; i-- {
		name := entries[i].Name()
		var at uint64
		path := filepath.Join(dir, name)
		if _, err := fmt.Sscanf(name, "%020d.ckpt", &at); err != nil {
			continue
		}
		if at > revision {
			stale = append(stale, path)
			continue
		}
		root, ckptEnd, err := readCheckpoint(path, at)
		if err != nil {
			stale = append(stale, path)
			continue
		}
		// The checkpoint's end must be where revision at+1 starts (or the
		// committed end when at is the newest revision).
		var expected int64
		if at == revision {
			expected = end
		} else if off, ok, err := idx.offset(at + 1); err == nil && ok {
			expected = off
		} else {
			stale = append(stale, path)
			continue
		}
		if ckptEnd != expected {
			stale = append(stale, path)
			continue
		}
		return root, at, ckptEnd, stale, nil
	}
	return nil, 0, 0, stale, nil
}

func readCheckpoint(path string, revision uint64) (root *treap, end int64, retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	if info.Size() < checkpointHeaderSize+12 {
		return nil, 0, storage.ErrCorrupt
	}
	crc := crc32.New(table)
	r := bufio.NewReaderSize(io.TeeReader(io.LimitReader(f, info.Size()-4), crc), 1<<16)
	var header [checkpointHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, 0, storage.ErrCorrupt
	}
	if string(header[:4]) != string(checkpointMagic[:]) || binary.BigEndian.Uint64(header[4:12]) != revision {
		return nil, 0, storage.ErrCorrupt
	}
	rawEnd := binary.BigEndian.Uint64(header[12:20])
	if rawEnd > uint64(^uint64(0)>>1) {
		return nil, 0, storage.ErrCorrupt
	}
	end = int64(rawEnd)
	remaining := info.Size() - checkpointHeaderSize - 12
	var count uint64
	var scratch [12]byte
	var prev string
	for remaining > 0 {
		if _, err := io.ReadFull(r, scratch[:2]); err != nil {
			return nil, 0, storage.ErrCorrupt
		}
		n := int(binary.BigEndian.Uint16(scratch[:2]))
		if int64(2+n+12) > remaining {
			return nil, 0, storage.ErrCorrupt
		}
		key := make([]byte, n)
		if _, err := io.ReadFull(r, key); err != nil {
			return nil, 0, storage.ErrCorrupt
		}
		if _, err := io.ReadFull(r, scratch[:12]); err != nil {
			return nil, 0, storage.ErrCorrupt
		}
		k := string(key)
		if count > 0 && k <= prev {
			return nil, 0, storage.ErrCorrupt
		}
		prev = k
		off := binary.BigEndian.Uint64(scratch[:8])
		length := binary.BigEndian.Uint32(scratch[8:12])
		if off > uint64(^uint64(0)>>1) || int64(off)+int64(length) > end {
			return nil, 0, storage.ErrCorrupt
		}
		// Keys arrive sorted, so building by merge keeps the tree balanced
		// without per-key splits.
		root = merge(root, newNode(k, recordRef{offset: int64(off), length: int32(length)}, nil, nil))
		count++
		remaining -= int64(2 + n + 12)
	}
	var trailer [8]byte
	if _, err := io.ReadFull(r, trailer[:]); err != nil || binary.BigEndian.Uint64(trailer[:]) != count {
		return nil, 0, storage.ErrCorrupt
	}
	var sum [4]byte
	if _, err := f.ReadAt(sum[:], info.Size()-4); err != nil || binary.BigEndian.Uint32(sum[:]) != crc.Sum32() {
		return nil, 0, storage.ErrCorrupt
	}
	return root, end, nil
}

// pruneCheckpoints removes checkpoints other than the newest keep.
func pruneCheckpoints(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		var at uint64
		if _, err := fmt.Sscanf(e.Name(), "%020d.ckpt", &at); err == nil {
			names = append(names, e.Name())
		}
	}
	for i := 0; i+keep < len(names); i++ {
		os.Remove(filepath.Join(dir, names[i]))
	}
}
