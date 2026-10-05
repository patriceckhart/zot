package journal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Cut a real frame at every byte, including its complete-but-unpublished form.
// The previous boundary must remain authoritative for every cut.
func TestEveryUnpublishedFrameCut(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	opts := Options{Durability: storage.Process}
	s, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	commitTest(t, s)
	boundary, err := os.ReadFile(filepath.Join(path, "boundary"))
	if err != nil {
		t.Fatal(err)
	}
	previous, err := os.ReadFile(filepath.Join(path, "commits.log"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Commit(ctx, storage.Mutation{
		ExpectedRevision: snap.Revision(), Epoch: s.Epoch(),
		Operations: []storage.Operation{
			{Key: "test", Delete: true},
			{Key: "unpublished", Value: json.RawMessage(`{"synthetic":"partial frame"}`)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	complete, err := os.ReadFile(filepath.Join(path, "commits.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for cut := len(previous); cut <= len(complete); cut++ {
		t.Run(strconv.Itoa(cut-len(previous)), func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(path, "commits.log"), complete[:cut], 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "boundary"), boundary, 0o600); err != nil {
				t.Fatal(err)
			}
			recovered, err := Open(ctx, path, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer recovered.Close()
			view, err := recovered.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			value, ok := view.Get("test")
			if !ok || string(value) != `"synthetic"` {
				t.Fatal("unpublished deletion changed acknowledged state")
			}
			if _, ok := view.Get("unpublished"); ok {
				t.Fatal("unpublished insertion became visible")
			}
			commits, err := recovered.Scan(ctx, snap.Revision(), 100)
			if err != nil || len(commits) != 1 || commits[0].Actor != "writer.acquire" {
				t.Fatalf("unpublished commit entered history: %v", err)
			}
			commitTest(t, recovered)
		})
	}
}

// A published frame or marker is never treated as a discardable tail, even
// when truncation leaves a valid prefix ending on an earlier frame boundary.
func TestEveryAcknowledgedTruncation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	opts := Options{Durability: storage.Process}
	s, err := Open(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	commitTest(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, "commits.log"))
	if err != nil {
		t.Fatal(err)
	}
	boundary, err := os.ReadFile(filepath.Join(path, "boundary"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"commits.log", "boundary"} {
		full := data
		if name == "boundary" {
			full = boundary
		}
		for cut := 0; cut < len(full); cut++ {
			t.Run(name+"/"+strconv.Itoa(cut), func(t *testing.T) {
				if err := os.WriteFile(filepath.Join(path, "commits.log"), data, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, "boundary"), boundary, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path, name), full[:cut], 0o600); err != nil {
					t.Fatal(err)
				}
				recovered, err := Open(ctx, path, opts)
				if recovered != nil {
					recovered.Close()
				}
				if !errors.Is(err, storage.ErrCorrupt) {
					t.Fatalf("acknowledged truncation accepted: %v", err)
				}
			})
		}
	}
}
