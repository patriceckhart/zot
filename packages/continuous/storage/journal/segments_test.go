package journal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func keyed(t *testing.T, s storage.Store, key, value string) storage.Commit {
	t.Helper()
	v, err := s.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Commit(context.Background(), storage.Mutation{ExpectedRevision: v.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: key, Value: json.RawMessage(value)}}})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// With a tiny segment size the log rotates into segments/; every record,
// the full history, verification, backup, restore, and migration still see
// one logical stream. Sealed segments are byte-identical after further
// writes.
func TestSegmentsRotateAndStayLogical(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	opts := Options{Durability: storage.Process, SegmentBytes: 400, CheckpointEvery: 7}
	s, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 40; i++ {
		keyed(t, s, fmt.Sprintf("k/%02d", i%10), fmt.Sprintf(`{"i":%d,"pad":"%s"}`, i, strings.Repeat("x", 40)))
	}
	sealed := journalFiles(t, path)
	segmentCount := 0
	for name := range sealed {
		if strings.HasPrefix(name, "segments/") {
			segmentCount++
		}
	}
	if segmentCount < 3 {
		t.Fatalf("expected rotation, got %d segments", segmentCount)
	}
	for i := 40; i < 50; i++ {
		keyed(t, s, fmt.Sprintf("k/%02d", i%10), fmt.Sprintf(`{"i":%d}`, i))
	}
	after := journalFiles(t, path)
	for name, b := range sealed {
		if name == "boundary" || strings.HasPrefix(name, "indexes/") || strings.HasPrefix(name, "checkpoints/") {
			continue
		}
		if next, ok := after[name]; ok && len(next) == len(b) && string(next) != string(b) {
			t.Fatalf("sealed file %s rewritten", name)
		}
		if next, ok := after[name]; ok && len(next) < len(b) {
			t.Fatalf("sealed file %s shrank", name)
		}
	}
	snap, _ := s.Snapshot(ctx)
	for i := 0; i < 10; i++ {
		b, ok := snap.Get(fmt.Sprintf("k/%02d", i))
		if !ok || !strings.Contains(string(b), fmt.Sprintf(`"i":%d`, 40+i)) {
			t.Fatalf("k/%02d: %s %v", i, b, ok)
		}
	}
	page, err := snap.Page("k/", "", 100)
	if err != nil || len(page) != 10 {
		t.Fatalf("page: %d %v", len(page), err)
	}
	var all []storage.Commit
	after2 := uint64(0)
	for {
		commits, err := s.Scan(ctx, after2, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(commits) == 0 {
			break
		}
		all = append(all, commits...)
		after2 = commits[len(commits)-1].Revision
	}
	if len(all) != 51 || all[50].Revision != 51 {
		t.Fatalf("history: %d", len(all))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := Verify(ctx, path)
	if err != nil || !report.Valid || report.Revision != 51 {
		t.Fatalf("verify: %+v %v", report, err)
	}
	home := t.TempDir()
	archive := filepath.Join(home, "a.zotbackup")
	if _, err := Backup(ctx, path, archive, opts); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(home, "restored")
	if _, err := Restore(ctx, archive, restored, opts); err != nil {
		t.Fatal(err)
	}
	migrated := filepath.Join(home, "migrated")
	if _, err := Migrate(ctx, path, migrated, opts); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{restored, migrated} {
		r, err := Open(ctx, p, opts)
		if err != nil {
			t.Fatal(err)
		}
		v, _ := r.Snapshot(ctx)
		if v.Revision() != 52 {
			t.Fatalf("%s revision: %d", p, v.Revision())
		}
		if b, ok := v.Get("k/03"); !ok || !strings.Contains(string(b), `"i":43`) {
			t.Fatalf("%s k/03: %s %v", p, b, ok)
		}
		r.Close()
	}
	// Reopen the segmented store: records and history intact, no replay of
	// the whole log needed (a checkpoint exists at close).
	s, err = Open(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, _ := s.Snapshot(ctx)
	if b, ok := v.Get("k/07"); !ok || !strings.Contains(string(b), `"i":47`) {
		t.Fatalf("after reopen k/07: %s %v", b, ok)
	}
	// A missing sealed segment fails closed.
	s.Close()
	entries, _ := os.ReadDir(filepath.Join(path, "segments"))
	if err := os.Remove(filepath.Join(path, "segments", entries[0].Name())); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, path, opts); !errors.Is(err, storage.ErrCorrupt) {
		t.Fatalf("missing segment accepted: %v", err)
	}
	if _, err := Verify(ctx, path); !errors.Is(err, storage.ErrCorrupt) {
		t.Fatalf("verify accepted missing segment: %v", err)
	}
}

// Checkpoints and the offset index are derived: deleting or corrupting
// them never loses data, open rebuilds them, and a checkpoint from a
// different journal (restored elsewhere) is discarded rather than trusted.
func TestDerivedIndexesAreRebuildable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	opts := Options{Durability: storage.Process, CheckpointEvery: 3}
	s, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		keyed(t, s, fmt.Sprintf("k/%d", i%4), fmt.Sprint(i))
	}
	v, _ := s.Snapshot(ctx)
	if _, err := s.Commit(ctx, storage.Mutation{ExpectedRevision: v.Revision(), Epoch: s.Epoch(), Operations: []storage.Operation{{Key: "k/1", Delete: true}}}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	want := map[string]string{"k/0": "8", "k/2": "6", "k/3": "7"}
	check := func(label string) {
		t.Helper()
		s, err := Open(ctx, path, opts)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		defer s.Close()
		v, _ := s.Snapshot(ctx)
		page, err := v.Page("k/", "", 10)
		if err != nil || len(page) != len(want) {
			t.Fatalf("%s page: %v %v", label, page, err)
		}
		for _, r := range page {
			if want[r.Key] != string(r.Value) {
				t.Fatalf("%s %s: %s", label, r.Key, r.Value)
			}
		}
		if _, ok := v.Get("k/1"); ok {
			t.Fatalf("%s: deleted key visible", label)
		}
		commits, err := s.Scan(ctx, 5, 3)
		if err != nil || len(commits) != 3 || commits[0].Revision != 6 {
			t.Fatalf("%s scan: %v %v", label, commits, err)
		}
	}
	check("intact")
	if entries, _ := os.ReadDir(filepath.Join(path, "checkpoints")); len(entries) == 0 {
		t.Fatal("no checkpoint written")
	}
	// Remove derived files.
	os.RemoveAll(filepath.Join(path, "checkpoints"))
	os.RemoveAll(filepath.Join(path, "indexes"))
	check("without derived files")
	// Corrupt the offset index and a checkpoint.
	for _, name := range []string{"indexes/offsets"} {
		b, err := os.ReadFile(filepath.Join(path, name))
		if err != nil {
			t.Fatal(err)
		}
		b[len(b)/2] ^= 0xff
		os.WriteFile(filepath.Join(path, name), b, 0o600)
	}
	entries, _ := os.ReadDir(filepath.Join(path, "checkpoints"))
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(path, "checkpoints", e.Name()))
		b[len(b)-1] ^= 0xff
		os.WriteFile(filepath.Join(path, "checkpoints", e.Name()), b, 0o600)
	}
	check("corrupt derived files")
	// A checkpoint claiming a revision beyond the journal is discarded.
	root := (*treap)(nil).put("ghost", recordRef{offset: 0, length: 1})
	if err := writeCheckpoint(filepath.Join(path, "checkpoints"), 99, 10, root, false); err != nil {
		t.Fatal(err)
	}
	check("stale checkpoint")
	// A truncated offset index (shorter than the journal) is extended.
	os.Truncate(filepath.Join(path, "indexes", "offsets"), offsetEntrySize*2)
	check("short offset index")
	// Shrinking the committed boundary below the checkpoint (simulating a
	// restore of an older backup over newer derived files) discards it.
	if _, err := Verify(ctx, path); err != nil {
		t.Fatal(err)
	}
}

// The persistent tree supports snapshots that outlive later commits and
// prefix iteration with cursors.
func TestTreapSemantics(t *testing.T) {
	var root *treap
	for i := 0; i < 500; i++ {
		root = root.put(fmt.Sprintf("k/%03d", (i*7919)%500), recordRef{offset: int64(i), length: 1})
	}
	if treapSize(root) != 500 {
		t.Fatalf("size: %d", treapSize(root))
	}
	before := root
	root = root.remove("k/010").put("k/010", recordRef{offset: 9999, length: 2}).remove("k/011")
	if ref, ok := before.get("k/011"); !ok || ref.length != 1 {
		t.Fatal("old root changed")
	}
	if ref, ok := root.get("k/010"); !ok || ref.offset != 9999 {
		t.Fatal("update lost")
	}
	if _, ok := root.get("k/011"); ok {
		t.Fatal("remove lost")
	}
	var keys []string
	root.ascend("k/49", func(key string, _ recordRef) bool {
		keys = append(keys, key)
		return len(keys) < 3
	})
	if len(keys) != 3 || keys[0] != "k/490" || keys[2] != "k/492" {
		t.Fatalf("ascend: %v", keys)
	}
	// Repeated puts of the same key keep one node: 500 keys, one removed,
	// one added.
	for i := 0; i < 10; i++ {
		root = root.put("dup", recordRef{offset: int64(i), length: 1})
	}
	if treapSize(root) != 500 {
		t.Fatalf("size after duplicate puts: %d", treapSize(root))
	}
	if ref, ok := root.get("dup"); !ok || ref.offset != 9 {
		t.Fatalf("last put not visible: %+v %v", ref, ok)
	}
}

// Recovery is bounded by the newest checkpoint: reopening a store with
// many commits reads a small suffix of the log, not every frame. Memory
// after open holds one index node per live key, not the history.
func TestOpenReplaysOnlyAfterCheckpoint(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	opts := Options{Durability: storage.Process, CheckpointEvery: 100, SegmentBytes: 1 << 20}
	s, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	const commits = 2000
	for i := 0; i < commits; i++ {
		keyed(t, s, fmt.Sprintf("entry/%05d", i), fmt.Sprintf(`{"n":%d,"pad":"%s"}`, i, strings.Repeat("y", 200)))
	}
	// Crash without Close: the newest periodic checkpoint is at most 100
	// commits behind the end.
	s.(*Store).closeFiles()
	segs, err := openSegments(path, true)
	if err != nil {
		t.Fatal(err)
	}
	size := segs.size()
	segs.close()
	s2, err := open(ctx, path, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	v, _ := s2.Snapshot(ctx)
	if v.Revision() != commits+2 {
		t.Fatalf("revision: %d", v.Revision())
	}
	// Prove bounded replay through the checkpoint position rather than
	// instrumentation: the newest checkpoint must be within 100 commits of
	// the end, and its recorded end within a small fraction of the log.
	root, at, end, _, err := loadCheckpoint(filepath.Join(path, "checkpoints"), s2.(*Store).offsets, commits+1, s2.(*Store).end)
	if err != nil || root == nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if commits+1-at > 100 || size-end > size/10 {
		t.Fatalf("checkpoint too far from end: at %d of %d, end %d of %d", at, commits+1, end, size)
	}
	if treapSize(root) != int(at)-1 {
		t.Fatalf("index entries: %d at revision %d", treapSize(root), at)
	}
	page, err := v.Page("entry/", "entry/01994", 10)
	if err != nil || len(page) != 5 || page[0].Key != "entry/01995" {
		t.Fatalf("page: %v %v", page, err)
	}
}
