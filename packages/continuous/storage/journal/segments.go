package journal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// The committed stream is one logical byte sequence stored in segments:
// commits.log holds logical offsets [0, size0), and segments/<start>.log
// holds [start, start+size). The boundary marker, offset index, and
// checkpoints all use logical offsets, so the frame format and every tool
// built on it are unchanged by segmentation. The writer appends to the
// last segment and rotates to a new file once it exceeds SegmentBytes;
// sealed segments are never written again, which makes them safe to copy
// or drop as units.

// DefaultSegmentBytes is the rotation threshold for new segments.
const DefaultSegmentBytes = 64 << 20

type segment struct {
	start int64
	size  int64
	path  string
	f     *os.File
}

type segmentSet struct {
	dir      string
	readOnly bool
	segs     []segment
}

// openSegments opens commits.log and any rotated segments. Sizes are taken
// from the files; the caller bounds reads by the committed end. In
// read-only mode files open without write access and nothing is created.
func openSegments(dir string, readOnly bool) (*segmentSet, error) {
	s := &segmentSet{dir: dir, readOnly: readOnly}
	flag := os.O_RDWR
	if readOnly {
		flag = os.O_RDONLY
	}
	first, err := os.OpenFile(filepath.Join(dir, "commits.log"), flag, 0)
	if err != nil {
		return nil, err
	}
	info, err := first.Stat()
	if err != nil {
		first.Close()
		return nil, err
	}
	s.segs = append(s.segs, segment{start: 0, size: info.Size(), path: first.Name(), f: first})
	entries, err := os.ReadDir(filepath.Join(dir, "segments"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.close()
		return nil, err
	}
	var starts []int64
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".log") || e.IsDir() {
			continue
		}
		start, err := strconv.ParseInt(strings.TrimSuffix(name, ".log"), 10, 64)
		if err != nil || start <= 0 {
			continue
		}
		starts = append(starts, start)
	}
	sort.Slice(starts, func(i, j int) bool { return starts[i] < starts[j] })
	for _, start := range starts {
		f, err := os.OpenFile(filepath.Join(dir, "segments", fmt.Sprintf("%020d.log", start)), flag, 0)
		if err != nil {
			s.close()
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			s.close()
			return nil, err
		}
		s.segs = append(s.segs, segment{start: start, size: info.Size(), path: f.Name(), f: f})
	}
	return s, nil
}

// size is the logical length of all bytes present, including any
// unacknowledged tail in the last segment.
func (s *segmentSet) size() int64 {
	last := s.segs[len(s.segs)-1]
	return last.start + last.size
}

// validate checks that segments are contiguous: each sealed segment must end
// exactly where the next starts. A gap or overlap means a file was removed
// or replaced outside this API.
func (s *segmentSet) validate(end int64) error {
	for i := 1; i < len(s.segs); i++ {
		prev := s.segs[i-1]
		if prev.start+prev.size != s.segs[i].start {
			return fmt.Errorf("%w: segment %s does not end where %s starts", errSegments, prev.path, s.segs[i].path)
		}
	}
	if s.size() < end {
		return fmt.Errorf("%w: segments shorter than committed boundary", errSegments)
	}
	return nil
}

var errSegments = fmt.Errorf("%w: segments", storage.ErrCorrupt)

// ReadAt reads logical bytes, spanning segments as needed. Reads past the
// present bytes return io.EOF.
func (s *segmentSet) ReadAt(p []byte, off int64) (int, error) {
	total := 0
	for len(p) > 0 {
		i := sort.Search(len(s.segs), func(i int) bool { return s.segs[i].start > off }) - 1
		if i < 0 {
			return total, io.EOF
		}
		seg := s.segs[i]
		within := off - seg.start
		if within >= seg.size {
			return total, io.EOF
		}
		n := int64(len(p))
		if n > seg.size-within {
			n = seg.size - within
		}
		read, err := seg.f.ReadAt(p[:n], within)
		total += read
		off += int64(read)
		p = p[read:]
		if err != nil && !(errors.Is(err, io.EOF) && int64(read) == n) {
			return total, err
		}
	}
	return total, nil
}

// append writes to the active segment at its current size.
func (s *segmentSet) append(b []byte) error {
	last := &s.segs[len(s.segs)-1]
	if err := writeAt(last.f, b, last.size); err != nil {
		return err
	}
	last.size += int64(len(b))
	return nil
}

func writeAt(f *os.File, b []byte, off int64) error {
	n, err := f.WriteAt(b, off)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

// truncate discards everything above the logical end: segments starting at
// or beyond it are removed, the one containing it is cut. It is used once
// at open to drop an unacknowledged tail.
func (s *segmentSet) truncate(end int64) error {
	for len(s.segs) > 1 && s.segs[len(s.segs)-1].start >= end {
		last := s.segs[len(s.segs)-1]
		if err := last.f.Close(); err != nil {
			return err
		}
		if err := os.Remove(last.path); err != nil {
			return err
		}
		s.segs = s.segs[:len(s.segs)-1]
	}
	last := &s.segs[len(s.segs)-1]
	within := end - last.start
	if within < last.size {
		if err := last.f.Truncate(within); err != nil {
			return err
		}
		last.size = within
	}
	return nil
}

// rotate seals the active segment and starts a new one at the logical end.
// The new file is created empty; the caller synchronizes the directory in
// strict mode. An empty active segment is reused rather than rotated.
func (s *segmentSet) rotate() error {
	last := s.segs[len(s.segs)-1]
	if last.size == 0 {
		return nil
	}
	start := last.start + last.size
	if err := os.MkdirAll(filepath.Join(s.dir, "segments"), 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.dir, "segments", fmt.Sprintf("%020d.log", start))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	s.segs = append(s.segs, segment{start: start, size: 0, path: path, f: f})
	return nil
}

// active is the file receiving appends.
func (s *segmentSet) active() *os.File { return s.segs[len(s.segs)-1].f }

// activeStart is the logical offset where the active segment begins.
func (s *segmentSet) activeStart() int64 { return s.segs[len(s.segs)-1].start }

// activeSize is the number of bytes present in the active segment.
func (s *segmentSet) activeSize() int64 { return s.segs[len(s.segs)-1].size }

// segmentsDir is where rotated segments live.
func (s *segmentSet) segmentsDir() string { return filepath.Join(s.dir, "segments") }

func (s *segmentSet) close() error {
	var errs []error
	for _, seg := range s.segs {
		if seg.f != nil {
			errs = append(errs, seg.f.Close())
		}
	}
	s.segs = nil
	return errors.Join(errs...)
}
