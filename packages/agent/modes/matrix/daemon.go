package matrix

import (
	"path/filepath"

	"github.com/patriceckhart/zot/packages/agent/modes/bot"
)

// PIDPath returns the location of the matrix bot's pid file.
func PIDPath(zotHome string) string { return filepath.Join(zotHome, "matrix.pid") }

// LogPath returns the matrix bot's log file (stdout+stderr of `start`).
func LogPath(zotHome string) string { return filepath.Join(zotHome, "logs", "matrix.log") }

// CryptoStorePath returns the sqlite E2EE key store location.
func CryptoStorePath(zotHome string) string {
	return filepath.Join(zotHome, "matrix-crypto", "store.db")
}

// IsRunning reports whether a matrix bot daemon process is alive.
func IsRunning(zotHome string) (int, bool, error) { return bot.IsRunningAt(PIDPath(zotHome)) }
