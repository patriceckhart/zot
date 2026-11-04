package core

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patriceckhart/zot/packages/provider"
)

// ErrSessionInUse indicates another writer holds a named transcript open.
var ErrSessionInUse = errors.New("session is already open in another writer")

// NormalizeSessionAlias returns the portable, case-insensitive lookup name.
func NormalizeSessionAlias(name string) (string, error) {
	if len(name) == 0 || len(name) > 64 {
		return "", fmt.Errorf("session name must contain 1 to 64 ASCII letters, digits, hyphens, or underscores")
	}
	for i, c := range name {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || i > 0 && (c == '-' || c == '_') {
			continue
		}
		return "", fmt.Errorf("session name must start with a letter or digit and contain only ASCII letters, digits, hyphens, or underscores")
	}
	return strings.ToLower(name), nil
}

var sessionAdjectives = strings.Fields("amber brave bright calm chatty clever cozy curious dapper eager fair gentle gracious happy jolly kind lively lucky merry nimble oblong patient quiet rapid ready silver steady sunny tidy vivid warm wise")
var sessionNouns = strings.Fields("badger birch brook cedar cloud comet crane dolphin echidna falcon fern finch fox garden heron island koala lake lark maple meadow mouse otter owl panda pebble pine raven river robin willow wren")

func randomSessionAlias(attempt int) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	name := sessionAdjectives[int(b[0])%len(sessionAdjectives)] + "-" + sessionNouns[int(b[1])%len(sessionNouns)]
	if attempt >= 8 {
		name += "-" + strings.ToLower(rand.Text()[:6])
	}
	return name
}

func aliasPath(root, cwd, name string) string {
	return filepath.Join(SessionsDir(root, cwd), "named-"+name+".jsonl")
}

// createAliasedFile reserves a name with exclusive creation while holding its
// writer lock. Lock sidecars persist so waiters never lock different inodes.
func createAliasedFile(root, cwd, name string) (*os.File, *os.File, string, error) {
	if err := os.MkdirAll(SessionsDir(root, cwd), 0755); err != nil {
		return nil, nil, "", err
	}
	generated := name == ""
	for attempt := 0; attempt < 128; attempt++ {
		if generated {
			name = randomSessionAlias(attempt)
		}
		path := aliasPath(root, cwd, name)
		lock, err := acquireSessionLock(path)
		if err != nil {
			if generated && errors.Is(err, ErrSessionInUse) {
				continue
			}
			return nil, nil, "", err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err == nil {
			return f, lock, name, nil
		}
		lock.Close()
		if generated && errors.Is(err, os.ErrExist) {
			continue
		}
		return nil, nil, "", err
	}
	return nil, nil, "", fmt.Errorf("could not allocate a unique session name")
}

// OpenNamedSession resumes an alias in this cwd or creates it if absent.
// The freshness result distinguishes creation from an empty resumed session.
func OpenNamedSession(root, cwd, name, providerName, model, version string) (*Session, []provider.Message, bool, error) {
	name, err := NormalizeSessionAlias(name)
	if err != nil {
		return nil, nil, false, err
	}
	path := aliasPath(root, cwd, name)
	s, msgs, err := OpenSession(path)
	if err == nil {
		if s.Meta.Alias != name || s.Meta.CWD != cwd {
			s.Close()
			return nil, nil, false, fmt.Errorf("named session metadata does not match its name or working directory")
		}
		return s, msgs, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, false, err
	}
	f, lock, alias, err := createAliasedFile(root, cwd, name)
	if errors.Is(err, os.ErrExist) {
		s, msgs, err = OpenSession(path)
		if err == nil && (s.Meta.Alias != name || s.Meta.CWD != cwd) {
			s.Close()
			return nil, nil, false, fmt.Errorf("named session metadata does not match its name or working directory")
		}
		return s, msgs, false, err
	}
	if err != nil {
		return nil, nil, false, err
	}
	s, err = initializeSession(f, lock, alias, cwd, providerName, model, version)
	return s, nil, true, err
}

func acquireSessionLock(path string) (*os.File, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, filepath.Base(path))+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockSessionFile(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("open session: %w", err)
	}
	return lock, nil
}

func pruneEmptySession(path string) {
	if sessionNeedsLock(path) {
		lock, err := acquireSessionLock(path)
		if err != nil {
			return
		}
		defer lock.Close()
	}
	if sessionHasNoMessages(path) {
		_ = os.Remove(path)
	}
}

func sessionNeedsLock(path string) bool {
	if strings.HasPrefix(filepath.Base(path), "named-") {
		return true
	}
	meta, err := readSessionMeta(path)
	return err == nil && meta.Alias != ""
}
