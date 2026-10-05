package journal

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func archiveFixture(t *testing.T, opts Options) (string, string) {
	t.Helper()
	home := t.TempDir()
	store := filepath.Join(home, "store")
	seedFaultStore(t, store, opts)
	archive := filepath.Join(home, "archive")
	if _, err := Backup(context.Background(), store, archive, opts); err != nil {
		t.Fatal(err)
	}
	return store, archive
}

func archiveOperation(ctx context.Context, operation, store, archive, destination string, opts Options, hook faultHook) (Verification, error) {
	if operation == "backup" {
		return backup(ctx, store, destination, opts, hook)
	}
	return restore(ctx, archive, destination, opts, hook)
}

func TestArchiveCrashChild(t *testing.T) {
	store := os.Getenv("ZOT_ARCHIVE_CRASH_STORE")
	if store == "" {
		return
	}
	_, err := archiveOperation(context.Background(), os.Getenv("ZOT_ARCHIVE_CRASH_OPERATION"), store, os.Getenv("ZOT_ARCHIVE_CRASH_ARCHIVE"), os.Getenv("ZOT_ARCHIVE_CRASH_DESTINATION"), Options{Durability: storage.Durability(os.Getenv("ZOT_ARCHIVE_CRASH_MODE"))}, func(point string) error {
		if point == os.Getenv("ZOT_ARCHIVE_CRASH_POINT") {
			os.Exit(crashExitCode)
		}
		return nil
	})
	t.Fatalf("archive crash checkpoint not reached: %v", err)
}

func TestArchiveFaultMatrix(t *testing.T) {
	ctx := context.Background()
	failure := errors.New("synthetic archive failure")
	for _, mode := range faultModes() {
		opts := Options{Durability: mode}
		for _, operation := range []string{"backup", "restore"} {
			store, archive := archiveFixture(t, opts)
			var points []string
			if _, err := archiveOperation(ctx, operation, store, archive, filepath.Join(t.TempDir(), "discovery"), opts, func(point string) error {
				points = append(points, point)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			publication := "after.backup.checksum.write"
			if operation == "restore" {
				publication = "after.restore.boundary.write"
			}
			complete := false
			seen := make(map[string]bool)
			for _, point := range points {
				if seen[point] {
					t.Fatalf("ambiguous archive checkpoint: %s", point)
				}
				seen[point] = true
				if point == publication {
					complete = true
				}
				for _, crash := range []bool{false, true} {
					kind := "error"
					if crash {
						kind = "crash"
					}
					t.Run(string(mode)+"/"+operation+"/"+kind+"/"+point, func(t *testing.T) {
						destination := filepath.Join(t.TempDir(), "destination")
						before := journalFiles(t, store)
						if crash {
							deadline, cancel := context.WithTimeout(ctx, 20*time.Second)
							defer cancel()
							cmd := exec.CommandContext(deadline, os.Args[0], "-test.run=^TestArchiveCrashChild$")
							cmd.Env = append(os.Environ(), "ZOT_ARCHIVE_CRASH_STORE="+store, "ZOT_ARCHIVE_CRASH_ARCHIVE="+archive, "ZOT_ARCHIVE_CRASH_DESTINATION="+destination, "ZOT_ARCHIVE_CRASH_MODE="+string(mode), "ZOT_ARCHIVE_CRASH_OPERATION="+operation, "ZOT_ARCHIVE_CRASH_POINT="+point)
							b, err := cmd.CombinedOutput()
							var exit *exec.ExitError
							if !errors.As(err, &exit) || exit.ExitCode() != crashExitCode {
								t.Fatalf("archive child: %v, %s", err, b)
							}
							var got Verification
							if operation == "backup" {
								got, err = VerifyBackup(ctx, destination)
							} else {
								got, err = Verify(ctx, destination)
							}
							if complete {
								want, verifyErr := Verify(ctx, store)
								if operation == "backup" {
									want.Scope = "journal-backup"
								}
								if err != nil || verifyErr != nil || got != want {
									t.Fatalf("complete crash destination changed: %+v, %v", got, err)
								}
							} else if err == nil {
								t.Fatal("partial crash destination passed verification")
							}
						} else {
							_, err := archiveOperation(ctx, operation, store, archive, destination, opts, func(p string) error {
								if p == point {
									return failure
								}
								return nil
							})
							if !errors.Is(err, failure) {
								t.Fatalf("archive failure not propagated: %v", err)
							}
							if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
								t.Fatalf("failed archive operation left destination: %v", err)
							}
						}
						assertJournalFiles(t, store, before)
					})
				}
			}
			if !seen[publication] {
				t.Fatal("archive publication checkpoint missing")
			}
		}
	}
}
