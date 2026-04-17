package tools

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// Sandbox guards tool access to the filesystem and shell. When Locked
// is true (1), file tools refuse paths outside Root and bash runs with
// a restricted environment.
//
// The value is designed to be shared across tool instances (by pointer).
// Enable/Disable are atomic so they can be toggled from the TUI.
type Sandbox struct {
	Root   string
	locked atomic.Bool
}

// NewSandbox returns a Sandbox rooted at cwd. It starts unlocked.
func NewSandbox(root string) *Sandbox {
	s := &Sandbox{Root: root}
	return s
}

// Lock enables sandboxing.
func (s *Sandbox) Lock() { s.locked.Store(true) }

// Unlock disables sandboxing.
func (s *Sandbox) Unlock() { s.locked.Store(false) }

// Locked reports whether the sandbox is enforcing limits.
func (s *Sandbox) Locked() bool { return s != nil && s.locked.Load() }

// CheckPath verifies that path resolves inside the sandbox root.
// Returns an error describing the violation if not. No-op when unlocked.
// Callers should pass an already-absolute path (use resolvePath() first).
func (s *Sandbox) CheckPath(path string) error {
	if !s.Locked() {
		return nil
	}
	rootAbs, err := canonical(s.Root)
	if err != nil {
		return fmt.Errorf("sandbox root: %w", err)
	}
	// Resolve the target to an absolute path. Walk up until we find an
	// existing parent so symlinks inside nonexistent dirs are still caught.
	target, err := canonicalOrParent(path)
	if err != nil {
		return fmt.Errorf("sandbox path: %w", err)
	}
	if !isUnder(rootAbs, target) {
		return fmt.Errorf("locked: path %q is outside sandbox root %q (use /unlock to disable)", path, s.Root)
	}
	return nil
}

// CheckCommand applies a lightweight sanity check to a bash command
// when locked. We cannot fully sandbox a shell, but we can reject the
// most obvious escapes so the model does not accidentally touch files
// outside root via absolute paths.
func (s *Sandbox) CheckCommand(cmd string) error {
	if !s.Locked() {
		return nil
	}
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return nil
	}
	// Reject obvious destructive roots.
	banned := []string{
		"rm -rf /", "rm -rf ~", "rm -rf $HOME",
		"sudo ", "su ",
		"chmod -R ", "chown -R ",
		"mkfs", "dd if=", "dd of=/",
	}
	lower := strings.ToLower(cmd)
	for _, b := range banned {
		if strings.Contains(lower, strings.ToLower(b)) {
			return fmt.Errorf("locked: command contains banned pattern %q (use /unlock to disable)", b)
		}
	}
	// Heuristic: reject a leading `cd /` or `cd ~` that tries to move
	// the shell out of the sandbox. Note this only catches simple cases;
	// a determined adversary can still escape. This is a speed bump for
	// the model, not a security boundary.
	first := strings.TrimSpace(strings.SplitN(cmd, ";", 2)[0])
	first = strings.TrimSpace(strings.SplitN(first, "&&", 2)[0])
	if strings.HasPrefix(first, "cd /") || strings.HasPrefix(first, "cd ~") ||
		strings.HasPrefix(first, "cd $HOME") || strings.HasPrefix(first, "cd ..") {
		return fmt.Errorf("locked: cd outside sandbox root is not allowed (use /unlock to disable)")
	}
	return nil
}

// canonical returns an absolute, symlink-resolved path. Errors on missing files.
func canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// canonicalOrParent returns the canonical path for p; if p doesn't exist,
// it walks up until it finds an existing directory, then appends the
// remaining path components. This catches symlink-escapes in non-existent
// subtrees (e.g. "new-file" inside a symlinked dir).
func canonicalOrParent(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	// If the full path exists, resolve it.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	// Otherwise, find the longest existing prefix.
	remaining := ""
	current := abs
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, remaining), nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return abs, nil
		}
		remaining = filepath.Join(filepath.Base(current), remaining)
		current = parent
	}
}

// isUnder reports whether target is equal to root or a descendant of it.
func isUnder(root, target string) bool {
	rootSep := root
	if !strings.HasSuffix(rootSep, string(filepath.Separator)) {
		rootSep += string(filepath.Separator)
	}
	return target == root || strings.HasPrefix(target, rootSep)
}
