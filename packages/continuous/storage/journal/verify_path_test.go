package journal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyRejectsNonregularFiles(t *testing.T) {
	for _, name := range []string{"writer.lock", "commits.log", "boundary"} {
		for _, kind := range []string{"directory", "symlink"} {
			t.Run(name+"/"+kind, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "store")
				s := openTest(t, path)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(path, name)
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if err := os.Mkdir(file, 0o700); err != nil {
						t.Fatal(err)
					}
				} else {
					target := filepath.Join(t.TempDir(), "target")
					if err := os.WriteFile(target, []byte("synthetic"), 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, file); err != nil {
						t.Skipf("symlink unavailable: %v", err)
					}
				}
				if _, err := Verify(context.Background(), path); err == nil {
					t.Fatalf("accepted %s as a journal file", kind)
				}
			})
		}
	}
}

func TestVerifyRejectsDirectorySymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	s := openTest(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	before := journalFiles(t, path)
	if _, err := Verify(context.Background(), link); err == nil {
		t.Fatal("accepted directory symlink")
	}
	assertJournalFiles(t, path, before)
}
