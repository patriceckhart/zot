package journal

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// journalFiles reads journal and derived files, keyed by path relative to
// the store. The writer lock is not journal data, and Windows forbids reading
// its locked byte while the store is open.
func journalFiles(t *testing.T, path string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || p == filepath.Join(path, "writer.lock") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(path, p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func assertJournalFiles(t *testing.T, path string, before map[string][]byte) {
	t.Helper()
	after := journalFiles(t, path)
	if len(before) != len(after) {
		t.Fatal("inspection changed directory contents")
	}
	for name, b := range before {
		if next, ok := after[name]; !ok || !bytes.Equal(b, next) {
			t.Fatalf("inspection changed file %s", name)
		}
	}
}

func TestVerifyReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	s := openTest(t, path)
	commitTest(t, s)
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	epoch := s.Epoch()
	if _, err := Verify(ctx, path); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("inspection bypassed active writer: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before := journalFiles(t, path)
	for i := 0; i < 2; i++ {
		report, err := Verify(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Valid || report.Scope != "journal" || report.Schema != 1 || report.Revision != snap.Revision() || report.Epoch != epoch || report.CommittedBytes != int64(len(before["commits.log"])) || report.UnacknowledgedTailBytes != 0 {
			t.Fatalf("unexpected verification: %+v", report)
		}
		assertJournalFiles(t, path, before)
	}
	// Neither an unacknowledged tail nor an interrupted temporary marker is
	// authoritative. Inspection reports the tail but never removes either.
	f, err := os.OpenFile(filepath.Join(path, "commits.log"), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("partial tail")); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".boundary-interrupted"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	before = journalFiles(t, path)
	report, err := Verify(ctx, path)
	if err != nil || report.Revision != snap.Revision() || report.Epoch != epoch || report.UnacknowledgedTailBytes != int64(len("partial tail")) {
		t.Fatalf("tail inspection: %+v, %v", report, err)
	}
	assertJournalFiles(t, path, before)
	next := openTest(t, path)
	defer next.Close()
	if next.Epoch() != epoch+1 {
		t.Fatal("inspection advanced writer epoch")
	}
}

func TestVerifyMissingAndCancelled(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "missing")
	if _, err := Verify(ctx, path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing store accepted: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("inspection created a directory")
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(ctx, path); !errors.Is(err, storage.ErrCorrupt) {
		t.Fatalf("empty directory accepted: %v", err)
	}
	assertJournalFiles(t, path, map[string][]byte{})
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Verify(cancelled, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	for _, name := range []string{"writer.lock", "commits.log", "boundary"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store")
			s := openTest(t, path)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(path, name)); err != nil {
				t.Fatal(err)
			}
			before := journalFiles(t, path)
			if _, err := Verify(ctx, path); !errors.Is(err, storage.ErrCorrupt) {
				t.Fatalf("missing file accepted: %v", err)
			}
			assertJournalFiles(t, path, before)
		})
	}
}

// Encode checksummed fixtures so transaction validation is tested independently
// of framing checks. All fixture values are synthetic.
func writeVerificationFixture(t *testing.T, path string, commits []storage.Commit) {
	t.Helper()
	var data []byte
	for _, c := range commits {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		h := make([]byte, headerSize)
		copy(h, magic[:])
		binary.BigEndian.PutUint32(h[4:8], uint32(len(b)))
		binary.BigEndian.PutUint64(h[8:16], c.Revision)
		binary.BigEndian.PutUint32(h[16:20], crc32.Checksum(b, table))
		binary.BigEndian.PutUint32(h[20:24], crc32.Checksum(h[:20], table))
		data = append(data, h...)
		data = append(data, b...)
	}
	marker := make([]byte, 20)
	binary.BigEndian.PutUint64(marker[:8], uint64(len(commits)))
	binary.BigEndian.PutUint64(marker[8:16], uint64(len(data)))
	binary.BigEndian.PutUint32(marker[16:20], crc32.Checksum(marker[:16], table))
	for name, b := range map[string][]byte{"commits.log": data, "boundary": marker} {
		if err := os.WriteFile(filepath.Join(path, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerifyInvalidTransactions(t *testing.T) {
	for name, invalid := range map[string]storage.Commit{
		"schema":           {Schema: 2, Revision: 2, Epoch: 2},
		"zero-epoch":       {Schema: 1, Revision: 2},
		"regressing-epoch": {Schema: 1, Revision: 2, Epoch: 1},
		"revision":         {Schema: 1, Revision: 3, Epoch: 2},
		"duplicate-keys":   {Schema: 1, Revision: 2, Epoch: 2, Operations: []storage.Operation{{Key: "a", Value: json.RawMessage(`1`)}, {Key: "a", Value: json.RawMessage(`2`)}}},
		"empty-key":        {Schema: 1, Revision: 2, Epoch: 2, Operations: []storage.Operation{{Value: json.RawMessage(`1`)}}},
		"missing-value":    {Schema: 1, Revision: 2, Epoch: 2, Operations: []storage.Operation{{Key: "a"}}},
		"delete-value":     {Schema: 1, Revision: 2, Epoch: 2, Operations: []storage.Operation{{Key: "a", Delete: true, Value: json.RawMessage(`1`)}}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store")
			s := openTest(t, path)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			writeVerificationFixture(t, path, []storage.Commit{{Schema: 1, Revision: 1, Epoch: 2}, invalid})
			before := journalFiles(t, path)
			before["commits.log"] = append(before["commits.log"], []byte("unacknowledged tail")...)
			if err := os.WriteFile(filepath.Join(path, "commits.log"), before["commits.log"], 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(context.Background(), path); !errors.Is(err, storage.ErrCorrupt) {
				t.Fatalf("invalid transaction accepted: %v", err)
			}
			assertJournalFiles(t, path, before)
			// Recovery uses exactly the same validator and must reject before
			// tail truncation or writer acquisition changes authority.
			next, err := Open(context.Background(), path, testOptions())
			if next != nil {
				next.Close()
			}
			if !errors.Is(err, storage.ErrCorrupt) {
				t.Fatalf("recovery accepted invalid transaction: %v", err)
			}
			assertJournalFiles(t, path, before)
		})
	}
}

func TestScanCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s := openTest(t, path)
	commitTest(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	segs, err := openSegments(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer segs.close()
	marker, err := readBoundary(filepath.Join(path, "boundary"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	visits := 0
	_, err = scanJournal(ctx, &journalData{segs}, marker, func(storage.Commit) error {
		visits++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || visits != 1 {
		t.Fatalf("scan ignored cancellation: %d, %v", visits, err)
	}
}
