package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/continuous/storage/sqlite"
)

// Store backends selectable with --backend. The journal is the default for
// new stores; existing stores are opened with the backend they were created
// with, detected from their files.
const (
	backendJournal = "journal"
	backendSQLite  = "sqlite"
)

// detectContinuousBackend reports which backend created the store at path,
// or "" for a directory that is not yet a store. A directory containing
// files of both backends is ambiguous and refused.
func detectContinuousBackend(path string) (string, error) {
	_, journalErr := os.Stat(filepath.Join(path, "boundary"))
	_, sqliteErr := os.Stat(filepath.Join(path, sqlite.DatabaseFile))
	switch {
	case journalErr == nil && sqliteErr == nil:
		return "", fmt.Errorf("continuous store %s contains both journal and sqlite files", path)
	case journalErr == nil:
		return backendJournal, nil
	case sqliteErr == nil:
		return backendSQLite, nil
	}
	return "", nil
}

// openContinuousStore opens the store at path. An explicit backend must
// match an existing store; an empty backend uses the detected one, or the
// journal for a new store.
func openContinuousStore(ctx context.Context, path, backend string, durability storage.Durability) (storage.Store, error) {
	existing, err := detectContinuousBackend(path)
	if err != nil {
		return nil, err
	}
	switch {
	case backend == "":
		backend = existing
		if backend == "" {
			backend = backendJournal
		}
	case existing != "" && existing != backend:
		return nil, fmt.Errorf("continuous store %s is a %s store, not %s", path, existing, backend)
	}
	switch backend {
	case backendJournal:
		return journal.Open(ctx, path, journal.Options{Durability: durability})
	case backendSQLite:
		return sqlite.Open(ctx, path, sqlite.Options{Durability: durability})
	}
	return nil, errors.New("backend must be journal or sqlite")
}

// validContinuousBackend reports whether name is a selectable backend.
func validContinuousBackend(name string) bool {
	return name == backendJournal || name == backendSQLite
}
