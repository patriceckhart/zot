package journal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestBackupRestore(t *testing.T) {
	for _, mode := range faultModes() {
		t.Run(string(mode), func(t *testing.T) {
			ctx := context.Background()
			home := t.TempDir()
			path := filepath.Join(home, "store")
			opts := Options{Durability: mode}
			seed := seedFaultStore(t, path, opts)
			// The backup must omit unacknowledged bytes, without trimming source.
			f, err := os.OpenFile(filepath.Join(path, "commits.log"), os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.Write([]byte("unacknowledged tail")); err != nil {
				f.Close()
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			before := journalFiles(t, path)
			archive := filepath.Join(home, "backup.zotbackup")
			report, err := Backup(ctx, path, archive, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !report.Valid || report.Scope != "journal-backup" || report.Revision != seed.submission.Revision || report.Epoch != seed.epoch || report.UnacknowledgedTailBytes != 0 {
				t.Fatalf("backup report: %+v", report)
			}
			assertJournalFiles(t, path, before)
			verified, err := VerifyBackup(ctx, archive)
			if err != nil || verified != report {
				t.Fatalf("archive verification: %+v, %v", verified, err)
			}
			destination := filepath.Join(home, "restored")
			restored, err := Restore(ctx, archive, destination, opts)
			if err != nil {
				t.Fatal(err)
			}
			want := report
			want.Scope = "journal"
			if restored != want {
				t.Fatalf("restore report: %+v", restored)
			}
			verified, err = Verify(ctx, destination)
			if err != nil || verified != want {
				t.Fatalf("restored verification: %+v, %v", verified, err)
			}
			files := journalFiles(t, destination)
			if !bytes.Equal(files["boundary"], before["boundary"]) || !bytes.Equal(files["commits.log"], before["commits.log"][:report.CommittedBytes]) {
				t.Fatal("restore changed committed source bytes")
			}
			if strictSupported {
				for _, target := range []string{archive, destination, filepath.Join(destination, "commits.log"), filepath.Join(destination, "boundary")} {
					info, err := os.Stat(target)
					if err != nil || info.Mode().Perm()&0o077 != 0 {
						t.Fatalf("unrestricted backup/restore permissions: %v", err)
					}
				}
			}
			s, err := Open(ctx, destination, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.Epoch() != seed.epoch+1 {
				t.Fatal("first restored writer did not advance epoch")
			}
			r, err := continuous.New(s)
			if err != nil {
				t.Fatal(err)
			}
			retry, err := r.Submit(ctx, seed.submission.ConversationID, "baseline", "accepted-key", "acknowledged input")
			if err != nil || !reflect.DeepEqual(retry, seed.submission) {
				t.Fatalf("restored deduplication changed admission: %v", err)
			}
			if _, err := r.Submit(ctx, seed.submission.ConversationID, "baseline", "accepted-key", "conflict"); !errors.Is(err, continuous.ErrRequestConflict) {
				t.Fatalf("restored conflicting retry accepted: %v", err)
			}
			assertJournalFiles(t, path, before)
		})
	}
}

func TestBackupRestoreRefusesOverwrite(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "store")
	opts := Options{Durability: storage.Process}
	seedFaultStore(t, path, opts)
	archive := filepath.Join(home, "backup")
	if _, err := Backup(ctx, path, archive, opts); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Backup(ctx, path, archive, opts); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwriting backup: %v", err)
	}
	after, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("existing archive changed")
	}
	before := journalFiles(t, path)
	if _, err := Restore(ctx, archive, path, opts); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwriting store: %v", err)
	}
	assertJournalFiles(t, path, before)
	if _, err := Backup(ctx, path, filepath.Join(path, "inside"), opts); err == nil {
		t.Fatal("backup allowed inside source store")
	}
	assertJournalFiles(t, path, before)
	s := openTest(t, path)
	defer s.Close()
	if _, err := Backup(ctx, path, filepath.Join(home, "locked"), opts); !errors.Is(err, storage.ErrLocked) {
		t.Fatalf("backup bypassed writer: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, operation := range []func() error{
		func() error { _, err := Backup(cancelled, path, filepath.Join(home, "cancelled"), opts); return err },
		func() error {
			_, err := Restore(cancelled, archive, filepath.Join(home, "cancelled"), opts)
			return err
		},
		func() error { _, err := VerifyBackup(cancelled, archive); return err },
	} {
		if err := operation(); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	}
}

func TestBackupEveryTruncationAndCorruption(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "store")
	opts := Options{Durability: storage.Process}
	seedFaultStore(t, path, opts)
	archive := filepath.Join(home, "backup")
	if _, err := Backup(ctx, path, archive, opts); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(home, "invalid")
	destination := filepath.Join(home, "must-not-create")
	check := func(t *testing.T, payload []byte) {
		t.Helper()
		if err := os.WriteFile(invalid, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyBackup(ctx, invalid); !errors.Is(err, storage.ErrCorrupt) {
			t.Fatalf("invalid archive accepted: %v", err)
		}
		if _, err := Restore(ctx, invalid, destination, opts); !errors.Is(err, storage.ErrCorrupt) {
			t.Fatalf("invalid archive restored: %v", err)
		}
		if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid archive created restore directory")
		}
	}
	for cut := 0; cut < len(b); cut++ {
		t.Run("cut/"+strconv.Itoa(cut), func(t *testing.T) { check(t, b[:cut]) })
	}
	for _, offset := range []int{0, 4, 12, 20, backupHeaderSize, len(b) - 1} {
		t.Run("flip/"+strconv.Itoa(offset), func(t *testing.T) {
			payload := bytes.Clone(b)
			payload[offset] ^= 1
			check(t, payload)
		})
	}
	check(t, append(bytes.Clone(b), 0))
	// A valid archive digest does not waive journal semantic validation.
	writeVerificationFixture(t, path, []storage.Commit{{Schema: 2, Revision: 1, Epoch: 1}})
	files := journalFiles(t, path)
	payload := append(append(append([]byte(nil), backupMagic[:]...), files["boundary"]...), files["commits.log"]...)
	sum := sha256.Sum256(payload)
	check(t, append(payload, sum[:]...))
}

func TestRestoreCleanupPreservesUnownedFiles(t *testing.T) {
	ctx := context.Background()
	opts := Options{Durability: storage.Process}
	_, archive := archiveFixture(t, opts)
	destination := filepath.Join(t.TempDir(), "restore")
	failure := errors.New("synthetic stop")
	_, err := restore(ctx, archive, destination, opts, func(point string) error {
		if point == "after.restore.directory.create" {
			if err := os.WriteFile(filepath.Join(destination, "writer.lock"), []byte("unowned synthetic file"), 0o600); err != nil {
				t.Fatal(err)
			}
			return failure
		}
		return nil
	})
	if !errors.Is(err, failure) {
		t.Fatalf("cleanup lost failure: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(destination, "writer.lock"))
	if err != nil || string(b) != "unowned synthetic file" {
		t.Fatal("cleanup removed unowned file")
	}
}

func TestArchiveDurabilityDoesNotDowngrade(t *testing.T) {
	for _, mode := range []storage.Durability{storage.Memory, "unsupported"} {
		if _, err := archiveDurability(Options{Durability: mode}); err == nil {
			t.Fatal("unsupported archive durability accepted")
		}
	}
	if !strictSupported {
		if _, err := archiveDurability(Options{}); err == nil {
			t.Fatal("strict archive durability downgraded")
		}
	}
}
