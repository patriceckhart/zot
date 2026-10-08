package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Backup copies the open database to a new file with SQLite's online backup
// API while the writer keeps working. The copy is a consistent snapshot of
// one committed revision. destination is a filesystem path, not a SQLite
// URI, and must not exist. It is created privately before any data is copied.
func (s *Store) Backup(ctx context.Context, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// SQLite accepts URI filenames. An absolute filesystem path prevents
	// it from interpreting a literal filename starting with "file:" as a URI.
	destination, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create sqlite backup: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(destination)
		}
	}()
	if err := f.Close(); err != nil {
		return err
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.reader == nil {
		return storage.ErrClosed
	}
	if err := s.reader.Backup("main", destination); err != nil {
		return fmt.Errorf("sqlite backup: %w", err)
	}
	ok = true
	return nil
}
