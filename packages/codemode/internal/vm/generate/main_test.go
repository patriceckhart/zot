package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerAssetExcludesVCSMetadata(t *testing.T) {
	output := filepath.Join(t.TempDir(), "worker.wasm.gz")
	if err := run(output, false); err != nil {
		t.Fatal(err)
	}
	asset, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(asset))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	wasm, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	// Revision and dirty-state stamping would change the asset after a commit
	// or an unrelated working-tree edit, even with identical worker sources.
	if bytes.Contains(wasm, []byte("build\tvcs")) {
		t.Fatal("worker asset contains VCS build metadata")
	}
	if err := run(output, true); err != nil {
		t.Fatalf("unchanged worker failed reproducibility check: %v", err)
	}
}

func TestCheckRejectsStaleAssetWithoutOverwriting(t *testing.T) {
	output := filepath.Join(t.TempDir(), "worker.wasm.gz")
	stale := []byte("stale worker asset")
	if err := os.WriteFile(output, stale, 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(output, true); err == nil || !strings.Contains(err.Error(), "codemode VM asset is stale") {
		t.Fatalf("expected stale asset error, got %v", err)
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, stale) {
		t.Fatal("check overwrote the stale worker asset")
	}
}
