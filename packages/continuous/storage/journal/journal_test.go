package journal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/conformance"
)

func testOptions() Options {
	if strictSupported {
		return Options{}
	}
	return Options{Durability: storage.Process}
}
func openTest(t *testing.T, path string) storage.Store {
	t.Helper()
	s, err := Open(context.Background(), path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) storage.Store { return openTest(t, filepath.Join(t.TempDir(), "store")) })
}
func commitTest(t *testing.T, s storage.Store) {
	t.Helper()
	v, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Commit(context.Background(), storage.Mutation{ExpectedRevision: v.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "test", Value: json.RawMessage(`"synthetic"`)}}})
	if err != nil {
		t.Fatal(err)
	}
}
func TestReopenAndFence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s := openTest(t, path)
	epoch := s.Epoch()
	commitTest(t, s)
	if _, err := Open(context.Background(), path, testOptions()); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("lock: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next := openTest(t, path)
	defer next.Close()
	if next.Epoch() <= epoch {
		t.Fatal("epoch did not advance")
	}
	v, _ := next.Snapshot(context.Background())
	b, ok := v.Get("test")
	if !ok || string(b) != `"synthetic"` {
		t.Fatal("committed value lost")
	}
	_, err := next.Commit(context.Background(), storage.Mutation{ExpectedRevision: v.Revision(), Epoch: epoch})
	if !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("old epoch: %v", err)
	}
}
func TestIncompleteUnacknowledgedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s := openTest(t, path)
	commitTest(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"commits.log", ".boundary-interrupted"} {
		f, err := os.OpenFile(filepath.Join(path, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("partial")); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	s = openTest(t, path)
	defer s.Close()
	v, _ := s.Snapshot(context.Background())
	if _, ok := v.Get("test"); !ok {
		t.Fatal("acknowledged state lost")
	}
	commitTest(t, s)
}
func TestAcknowledgedCorruptionFailsClosed(t *testing.T) {
	for _, mode := range []string{"truncate", "payload", "header", "barrier", "boundary-truncate"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store")
			s := openTest(t, path)
			commitTest(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(path, "commits.log")
			if mode == "barrier" || mode == "boundary-truncate" {
				file = filepath.Join(path, "boundary")
			}
			b, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "truncate", "boundary-truncate":
				b = b[:len(b)-1]
			case "payload":
				b[len(b)-1] ^= 1
			case "header":
				b[4] ^= 1
			case "barrier":
				b[0] ^= 1
			}
			if err := os.WriteFile(file, b, 0o600); err != nil {
				t.Fatal(err)
			}
			before := journalFiles(t, path)
			if _, err := Verify(context.Background(), path); !errors.Is(err, storage.ErrCorrupt) {
				t.Fatalf("verification accepted corruption: %v", err)
			}
			assertJournalFiles(t, path, before)
			if s, err := Open(context.Background(), path, testOptions()); !errors.Is(err, storage.ErrCorrupt) {
				if s != nil {
					s.Close()
				}
				t.Fatalf("corruption accepted: %v", err)
			}
		})
	}
}

func TestMissingJournalFileFailsClosed(t *testing.T) {
	for _, name := range []string{"commits.log", "boundary"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store")
			s := openTest(t, path)
			commitTest(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(path, name)); err != nil {
				t.Fatal(err)
			}
			s, err := Open(context.Background(), path, testOptions())
			if s != nil {
				s.Close()
			}
			if !errors.Is(err, storage.ErrCorrupt) {
				t.Fatalf("missing file accepted: %v", err)
			}
		})
	}
}

// The child exits without Close, exercising real OS writer release and an
// acknowledged commit surviving process termination rather than clean shutdown.
func TestProcessCrash(t *testing.T) {
	if path := os.Getenv("ZOT_CONTINUOUS_CRASH_TEST"); path != "" {
		s := openTest(t, path)
		commitTest(t, s)
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "store")
	cmd := exec.Command(os.Args[0], "-test.run=^TestProcessCrash$")
	cmd.Env = append(os.Environ(), "ZOT_CONTINUOUS_CRASH_TEST="+path)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v, %s", err, b)
	}
	s := openTest(t, path)
	defer s.Close()
	v, _ := s.Snapshot(context.Background())
	if _, ok := v.Get("test"); !ok {
		t.Fatal("process crash lost accepted commit")
	}
}

func TestStrictDoesNotDowngrade(t *testing.T) {
	if strictSupported {
		t.Skip("strict supported")
	}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "store"), Options{})
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("unsupported strict silently downgraded")
	}
}
func BenchmarkCommit(b *testing.B) {
	s, err := Open(context.Background(), filepath.Join(b.TempDir(), "store"), testOptions())
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	v, _ := s.Snapshot(ctx)
	rev := v.Revision()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: rev, Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "bench", Value: json.RawMessage(`1`)}}})
		if err != nil {
			b.Fatal(err)
		}
		rev = c.Revision
	}
}
