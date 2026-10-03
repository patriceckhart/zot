// Command generate builds the reproducible, compressed codemode VM asset.
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func main() {
	output := flag.String("output", "worker.wasm.gz", "asset path")
	check := flag.Bool("check", false, "check that the asset matches its sources")
	flag.Parse()
	if err := run(*output, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(output string, check bool) error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return fmt.Errorf("repository go.mod not found")
		}
		root = parent
	}
	dir, err := os.MkdirTemp("", "zot-codemode-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	wasm := filepath.Join(dir, "worker.wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w -buildid=", "-o", wasm, "./packages/codemode/internal/vm/cmd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	data, err := os.ReadFile(wasm)
	if err != nil {
		return err
	}
	var compressed bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
	if _, err := writer.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if check {
		old, err := os.ReadFile(output)
		if err != nil {
			return err
		}
		if !bytes.Equal(old, compressed.Bytes()) {
			return fmt.Errorf("codemode VM asset is stale, run go generate ./packages/codemode")
		}
		return nil
	}
	return os.WriteFile(output, compressed.Bytes(), 0644)
}
