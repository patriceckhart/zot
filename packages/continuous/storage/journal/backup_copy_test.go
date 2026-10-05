package journal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

type archiveTestWriter func([]byte) (int, error)

func (f archiveTestWriter) Write(b []byte) (int, error) { return f(b) }

func TestArchiveCopyContext(t *testing.T) {
	payload := bytes.Repeat([]byte("synthetic"), 20000)
	var out bytes.Buffer
	if err := copyContext(context.Background(), &out, bytes.NewReader(payload), int64(len(payload))); err != nil || !bytes.Equal(payload, out.Bytes()) {
		t.Fatalf("multi-chunk copy: %v", err)
	}
	if err := copyContext(context.Background(), io.Discard, bytes.NewReader(payload), int64(len(payload)+1)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short input: %v", err)
	}
	if err := copyContext(context.Background(), archiveTestWriter(func(b []byte) (int, error) { return len(b) - 1, nil }), bytes.NewReader(payload), int64(len(payload))); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	err := copyContext(ctx, archiveTestWriter(func(b []byte) (int, error) {
		calls++
		cancel()
		return len(b), nil
	}), bytes.NewReader(payload), int64(len(payload)))
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("copy ignored cancellation: %d, %v", calls, err)
	}
}

func TestArchiveCancellationCleansDestination(t *testing.T) {
	opts := Options{Durability: storage.Process}
	store, archive := archiveFixture(t, opts)
	before := journalFiles(t, store)
	for _, operation := range []string{"backup", "restore"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			destination := filepath.Join(t.TempDir(), "destination")
			point := "before.backup.data.write"
			if operation == "restore" {
				point = "before.restore.commits.log.write"
			}
			_, err := archiveOperation(ctx, operation, store, archive, destination, opts, func(p string) error {
				if p == point {
					cancel()
				}
				return nil
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("archive cancellation: %v", err)
			}
			if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("cancelled archive operation left destination")
			}
			assertJournalFiles(t, store, before)
		})
	}
}
