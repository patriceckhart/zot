package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

// Backup copies the open database to a new file with SQLite's online backup
// API while the writer keeps working. The copy is a consistent snapshot of
// one committed revision. destination must not exist.
func (s *Store) Backup(ctx context.Context, destination string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("backup destination %s exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if s.reader == nil {
		return storage.ErrClosed
	}
	if err := s.reader.Backup("main", destination); err != nil {
		os.Remove(destination)
		return fmt.Errorf("sqlite backup: %w", err)
	}
	return os.Chmod(destination, 0o600)
}
