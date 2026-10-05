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

func TestContinuousCLIBackupRestore(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "store")
	s, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := continuous.New(s)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.OpenRoot(ctx, "workspace", continuous.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	original, err := r.Submit(ctx, c.ID, "actor", "stable-request", "private synthetic input")
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(home, "archive")
	args := []string{"backup", archive, "--store", path, "--durability", "process"}
	var out bytes.Buffer
	if err := runContinuous(ctx, args, &out); !errors.Is(err, storage.ErrLocked) || out.Len() != 0 {
		t.Fatalf("backup bypassed writer: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) journal.Verification {
		t.Helper()
		out.Reset()
		if err := runContinuous(ctx, args, &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "private synthetic input") {
			t.Fatal("backup acknowledgement printed transcript")
		}
		var report journal.Verification
		if err := json.Unmarshal(out.Bytes(), &report); err != nil || !report.Valid {
			t.Fatalf("invalid archive report: %v", err)
		}
		return report
	}
	report := run(args...)
	if verified := run("verify-backup", archive); verified != report {
		t.Fatal("backup and verification revisions differ")
	}
	destination := filepath.Join(home, "restored")
	restored := run("restore", archive, "--store", destination, "--durability", "process")
	if restored.Revision != report.Revision || restored.Epoch != report.Epoch || restored.Scope != "journal" {
		t.Fatal("CLI restore changed archived revision")
	}
	if verified := run("verify", "--store", destination); verified != restored {
		t.Fatal("restored verification differs")
	}
	s, err = journal.Open(ctx, destination, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err = continuous.New(s)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := r.Submit(ctx, c.ID, "actor", "stable-request", "private synthetic input")
	if err != nil || retry != original {
		t.Fatalf("CLI restore changed deduplication: %v", err)
	}
	out.Reset()
	if err := runContinuous(ctx, args, &out); !errors.Is(err, os.ErrExist) || out.Len() != 0 {
		t.Fatalf("CLI backup overwrote existing archive: %v", err)
	}
	out.Reset()
	if err := runContinuous(ctx, []string{"restore", archive, "--store", destination, "--durability", "process"}, &out); !errors.Is(err, os.ErrExist) || out.Len() != 0 {
		t.Fatalf("CLI restore overwrote store: %v", err)
	}
}

// format reports the schema read-only; migrate rewrites into a new directory
// whose records match the source, keeping the source unchanged.
func TestContinuousCLIFormatMigrate(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "store")
	s, err := journal.Open(ctx, path, journal.Options{Durability: storage.Process})
	if err != nil {
		t.Fatal(err)
	}
	r, err := continuous.New(s)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.OpenRoot(ctx, "workspace", continuous.AgentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Submit(ctx, c.ID, "actor", "stable-request", "private synthetic input"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runContinuous(ctx, []string{"format", "--store", path, "--durability", "process"}, &out); err == nil {
		t.Fatal("format accepted --durability")
	}
	out.Reset()
	if err := runContinuous(ctx, []string{"format", "--store", path}, &out); err != nil {
		t.Fatal(err)
	}
	var format journal.Format
	if err := json.Unmarshal(out.Bytes(), &format); err != nil || !format.Current || format.Schema != journal.CurrentSchema || format.Revision == 0 {
		t.Fatalf("format: %s %v", out.String(), err)
	}
	if err := runContinuous(ctx, []string{"migrate", "--store", path, "--durability", "process"}, &out); err == nil {
		t.Fatal("migrate without destination accepted")
	}
	target := filepath.Join(home, "migrated")
	out.Reset()
	if err := runContinuous(ctx, []string{"migrate", target, "--store", path, "--durability", "process"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "private synthetic input") {
		t.Fatal("migration report printed transcript")
	}
	var report journal.Verification
	if err := json.Unmarshal(out.Bytes(), &report); err != nil || !report.Valid || report.Revision != format.Revision {
		t.Fatalf("migration report: %s %v", out.String(), err)
	}
	for _, store := range []string{path, target} {
		out.Reset()
		if err := runContinuous(ctx, []string{"conversations", "--store", store, "--durability", "process"}, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), c.ID) {
			t.Fatalf("conversation missing from %s: %s", store, out.String())
		}
	}
}
