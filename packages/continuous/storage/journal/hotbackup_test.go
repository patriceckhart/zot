package journal

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestHotBackupWhileOpen(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "store")
	st, err := Open(ctx, dir, Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	commit := func(i int) {
		snap, _ := st.Snapshot(ctx)
		v, _ := json.Marshal(i)
		if _, err := st.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: st.Epoch(),
			Operations: []storage.Operation{{Key: fmt.Sprintf("k/%d", i), Value: v}}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		commit(i)
	}
	archive := filepath.Join(t.TempDir(), "a.zotbackup")
	report, err := st.(*Store).Backup(ctx, archive, Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	commit(99) // the writer keeps working after the backup
	verified, err := VerifyBackup(ctx, archive)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Revision != report.Revision || report.Revision != 21 {
		t.Fatalf("revisions %d %d", verified.Revision, report.Revision)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if _, err := Restore(ctx, archive, restored, Options{Durability: storage.Process}); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, restored, Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	snap, _ := r.Snapshot(ctx)
	if _, ok := snap.Get("k/19"); !ok {
		t.Fatal("restored store misses committed record")
	}
	if _, ok := snap.Get("k/99"); ok {
		t.Fatal("restored store contains a commit after the backup")
	}
}
