package journal

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestDiskHistoryPagesAndAppendCursor(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	s, err := Open(ctx, path, Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	expected, err := s.Scan(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 23; i++ {
		snap, _ := s.Snapshot(ctx)
		op := storage.Operation{Key: "replaced", Value: json.RawMessage(`{"synthetic":true}`)}
		if i%3 == 0 {
			op = storage.Operation{Key: "replaced", Delete: true}
		}
		c, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{op}})
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, c)
		// Positional history reads must not move the next append to an earlier frame.
		if _, err := s.Scan(ctx, 0, 1); err != nil {
			t.Fatal(err)
		}
	}
	check := func() {
		t.Helper()
		var actual []storage.Commit
		var after uint64
		for {
			page, err := s.Scan(ctx, after, 5)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			actual = append(actual, page...)
			after = page[len(page)-1].Revision
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatal("disk history differs from acknowledged commits")
		}
	}
	check()
	rows, err := s.Scan(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	rows[0].Operations[0].Key = "mutated"
	rows, err = s.Scan(ctx, 1, 1)
	if err != nil || rows[0].Operations[0].Key == "mutated" {
		t.Fatal("scan results alias retained state")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	acquire, err := s.Scan(ctx, uint64(len(expected)), 1)
	if err != nil || len(acquire) != 1 {
		t.Fatalf("reacquisition: %+v %v", acquire, err)
	}
	expected = append(expected, acquire[0])
	check()
	if !s.Capabilities().BoundedHistoryMemory || !s.Capabilities().Indexed {
		t.Fatalf("capabilities: %+v", s.Capabilities())
	}
}

type historyReader struct {
	io.ReaderAt
	reads    int
	boundary int64
	cancel   context.CancelFunc
}

func (r *historyReader) ReadAt(p []byte, off int64) (int, error) {
	r.reads++
	if r.boundary > 0 && off+int64(len(p)) > r.boundary {
		return 0, io.ErrUnexpectedEOF
	}
	n, err := r.ReaderAt.ReadAt(p, off)
	if r.cancel != nil {
		r.cancel()
	}
	return n, err
}

func TestDiskHistoryPageBoundsAndFailures(t *testing.T) {
	path := t.TempDir()
	commits := []storage.Commit{
		{Schema: 1, Revision: 1, Epoch: 1},
		{Schema: 1, Revision: 2, Epoch: 1, Operations: []storage.Operation{{Key: "record", Value: json.RawMessage(`1`)}}},
		{Schema: 1, Revision: 3, Epoch: 1},
	}
	writeVerificationFixture(t, path, commits)
	data, err := os.ReadFile(filepath.Join(path, "commits.log"))
	if err != nil {
		t.Fatal(err)
	}
	end := int64(len(data))
	firstEnd := int64(headerSize) + int64(binary.BigEndian.Uint32(data[4:8]))
	reader := &historyReader{ReaderAt: bytes.NewReader(data), boundary: firstEnd}
	rows, err := scanPage(context.Background(), reader, end, 3, 0, 1)
	if err != nil || len(rows) != 1 || rows[0].Revision != 1 || reader.reads != 2 {
		t.Fatalf("read beyond requested page: %+v %v reads=%d", rows, err, reader.reads)
	}
	if rows, err := scanPage(context.Background(), reader, end, 3, 0, 2); !errors.Is(err, storage.ErrCorrupt) || len(rows) != 0 {
		t.Fatalf("partial success after read failure: %+v %v", rows, err)
	}
	reader = &historyReader{ReaderAt: bytes.NewReader(data)}
	if rows, err := scanPage(context.Background(), reader, end, 3, 3, 1); err != nil || len(rows) != 0 || reader.reads != 0 {
		t.Fatal("empty page read disk")
	}
	if _, err := scanPage(context.Background(), reader, end, 3, 4, 1); !errors.Is(err, storage.ErrCursor) || reader.reads != 0 {
		t.Fatal("invalid cursor read disk")
	}
	if _, err := scanPage(context.Background(), reader, end, 3, 0, 0); err == nil || reader.reads != 0 {
		t.Fatal("invalid limit read disk")
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader.cancel = cancel
	if rows, err := scanPage(ctx, reader, end, 3, 0, 3); !errors.Is(err, context.Canceled) || len(rows) != 0 {
		t.Fatalf("cancelled disk scan: %+v %v", rows, err)
	}
	// Historical corruption before the cursor cannot be silently skipped.
	damaged := bytes.Clone(data)
	damaged[headerSize] ^= 1
	if rows, err := scanPage(context.Background(), bytes.NewReader(damaged), end, 3, 1, 1); !errors.Is(err, storage.ErrCorrupt) || len(rows) != 0 {
		t.Fatalf("skipped corrupted prefix: %+v %v", rows, err)
	}
	// Never inspect or acknowledge bytes beyond the committed boundary.
	withTail := append(bytes.Clone(data), []byte("unpublished tail")...)
	rows, err = scanPage(context.Background(), bytes.NewReader(withTail), end, 3, 1, 10)
	if err != nil || !reflect.DeepEqual(rows, commits[1:]) {
		t.Fatalf("unpublished tail affected page: %+v %v", rows, err)
	}
	// A final page must still check that authority ends at its final frame,
	// even when the caller's limit exactly equals the number returned.
	if rows, err := scanPage(context.Background(), bytes.NewReader(withTail), int64(len(withTail)), 3, 1, 2); !errors.Is(err, storage.ErrCorrupt) || len(rows) != 0 {
		t.Fatalf("accepted mismatched committed end: %+v %v", rows, err)
	}
}

func TestHistoryVisitorError(t *testing.T) {
	path := t.TempDir()
	writeVerificationFixture(t, path, []storage.Commit{{Schema: 1, Revision: 1, Epoch: 1}})
	segs, err := openSegments(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer segs.close()
	marker, err := readBoundary(filepath.Join(path, "boundary"))
	if err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic replay failure")
	if _, err := scanJournal(context.Background(), &journalData{segs}, marker, func(storage.Commit) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("visitor failure lost: %v", err)
	}
}

func TestDiskHistoryConcurrentScan(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store"), Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const count = 40
	result := make(chan error, 1)
	go func() {
		var after uint64
		for after < count+1 {
			page, err := s.Scan(ctx, after, 3)
			if err != nil {
				result <- err
				return
			}
			if len(page) == 0 {
				if err := s.Wait(ctx, after); err != nil {
					result <- err
					return
				}
				continue
			}
			for _, c := range page {
				if c.Revision != after+1 {
					result <- fmt.Errorf("scan gap after %d", after)
					return
				}
				after = c.Revision
			}
		}
		result <- nil
	}()
	for i := 0; i < count; i++ {
		snap, err := s.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "replaced", Value: json.RawMessage(fmt.Sprint(i))}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := <-result; err != nil {
		t.Fatalf("concurrent disk history: %v", err)
	}
}
