package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
)

func TestContinuousCLIVerify(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store")
	s, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := continuous.New(s)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.OpenRoot(ctx, "synthetic workspace", continuous.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, c.ID, "local", "request", "private synthetic input"); err != nil {
		t.Fatal(err)
	}
	args := []string{"verify", "--store", path}
	var out bytes.Buffer
	if err := runContinuous(ctx, args, &out); !errors.Is(err, storage.ErrLocked) || out.Len() != 0 {
		t.Fatalf("verify bypassed writer or emitted success: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, "commits.log"))
	if err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(filepath.Join(path, "boundary"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := journal.Verify(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out.Reset()
		if err := runContinuous(ctx, args, &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "private synthetic input") {
			t.Fatal("verify included transcript content")
		}
		var got journal.Verification
		if err := json.Unmarshal(out.Bytes(), &got); err != nil || got != want {
			t.Fatalf("verify report: %+v, %v", got, err)
		}
		for name, expected := range map[string][]byte{"commits.log": data, "boundary": marker} {
			b, err := os.ReadFile(filepath.Join(path, name))
			if err != nil || !bytes.Equal(b, expected) {
				t.Fatalf("verify changed %s: %v", name, err)
			}
		}
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(filepath.Join(path, "commits.log"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runContinuous(ctx, args, &out); !errors.Is(err, storage.ErrCorrupt) || out.Len() != 0 {
		t.Fatalf("corrupt verify emitted success: %v", err)
	}
}

func TestContinuousCLIVerifyMissingStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	args := []string{"verify", "--store", path}
	if err := runContinuous(context.Background(), args, &bytes.Buffer{}); err == nil {
		t.Fatal("missing store accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verify created store")
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := runContinuous(context.Background(), args, &bytes.Buffer{}); err == nil {
		t.Fatal("empty directory accepted")
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) != 0 {
		t.Fatal("verify initialized directory")
	}
}
