package sqlite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestBackupReservesPrivateDestination(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "store"), Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	s := st.(*Store)
	t.Cleanup(func() { s.Close() })
	out := filepath.Join(t.TempDir(), "backup.sqlite")

	// Hold the reader so the backup cannot start copying. Its reservation
	// must already prevent other callers from using the destination.
	s.readMu.Lock()
	locked := true
	done := make(chan error, 1)
	go func() { done <- s.Backup(context.Background(), out) }()
	t.Cleanup(func() {
		if locked {
			s.readMu.Unlock()
			<-done
		}
	})
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		info, err := os.Stat(out)
		if err == nil {
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Fatalf("destination mode before copying: %o", info.Mode().Perm())
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("backup did not reserve destination before waiting for reader")
		}
	}
	if err := s.Backup(context.Background(), out); !errors.Is(err, os.ErrExist) {
		t.Fatalf("concurrent backup must reject reserved destination: %v", err)
	}
	s.readMu.Unlock()
	locked = false
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestBackupFailureRemovesReservation(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "store"), Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := st.(*Store).Backup(context.Background(), out); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("closed store backup: %v", err)
	}
	if _, err := os.Lstat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed backup left destination: %v", err)
	}
}

func TestBackupLiteralFilename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot contain a colon")
	}
	t.Chdir(t.TempDir())
	st, err := Open(context.Background(), "store", Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.(*Store).Backup(context.Background(), "file:backup.sqlite"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("file:backup.sqlite"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("backup.sqlite"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("literal filename interpreted as URI: %v", err)
	}
}

func TestBackupRejectsExistingDestination(t *testing.T) {
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "store"), Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := st.(*Store)
	out := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := s.Backup(context.Background(), out); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Commit(context.Background(), storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "new", Value: []byte(`true`)}}}); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{out, "file:" + filepath.ToSlash(out)} {
		if err := s.Backup(context.Background(), destination); err == nil {
			t.Fatalf("backup accepted existing destination %q", destination)
		}
		after, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("backup overwrote existing destination via %q", destination)
		}
	}
}
