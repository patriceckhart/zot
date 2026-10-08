package sqlite

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestHotBackupWhileOpen(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store"), Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	snap, _ := st.Snapshot(ctx)
	v, _ := json.Marshal("x")
	if _, err := st.Commit(ctx, storage.Mutation{ExpectedRevision: snap.Revision(), Epoch: st.Epoch(),
		Operations: []storage.Operation{{Key: "k", Value: v}}}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "copy")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := st.(*Store).Backup(ctx, filepath.Join(out, DatabaseFile)); err != nil {
		t.Fatal(err)
	}
	st.Close()
	copyStore, err := Open(ctx, out, Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer copyStore.Close()
	s2, _ := copyStore.Snapshot(ctx)
	if _, ok := s2.Get("k"); !ok {
		t.Fatal("backup misses record")
	}
}
