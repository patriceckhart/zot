package tui

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeFiles creates files (and intermediate dirs) under base for each
// relative path in rels. Returns base for convenience.
func makeFiles(t *testing.T, base string, rels ...string) string {
	t.Helper()
	for _, rel := range rels {
		full := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	return base
}

func TestQuotePastedFilePaths(t *testing.T) {
	dir := t.TempDir()
	makeFiles(t, dir,
		"foo bar.png",
		"file.png",
		"a.png",
		"b.png",
		"x.png",
		"it's.png",
		"only.png",
	)

	// file:// URL form needs URL-encoded spaces.
	fileURL := "file://" + dir + "/" + url.PathEscape("foo bar.png")
	// PathEscape doesn't escape "/" but escapes the space — that's
	// what macOS Finder produces. Build the path-only segment so the
	// "file://" prefix is added cleanly.
	fileURLPath := dir + "/" + strings.ReplaceAll("foo bar.png", " ", "%20")
	fileURLForm := "file://" + fileURLPath
	_ = fileURL // keep linter happy if unused

	cases := []struct {
		name string
		in   string
		want string
	}{
		// macOS Terminal default: backslash-escaped spaces in path.
		{"backslash-escaped space", dir + `/foo\ bar.png`, `'` + dir + `/foo bar.png'`},

		// Plain absolute path with no special chars.
		{"plain absolute", dir + `/file.png`, `'` + dir + `/file.png'`},

		// file:// URL form; URL-decoded and scheme stripped.
		{"file url", fileURLForm, `'` + dir + `/foo bar.png'`},

		// Multi-file drop, each path quoted independently.
		{"two paths", dir + `/a.png ` + dir + `/b.png`, `'` + dir + `/a.png' '` + dir + `/b.png'`},

		// Already single-quoted: re-normalise to consistent quoting.
		{"already single quoted", `'` + dir + `/x.png'`, `'` + dir + `/x.png'`},

		// Already double-quoted: re-normalise.
		{"already double quoted", `"` + dir + `/x.png"`, `'` + dir + `/x.png'`},

		// Embedded apostrophe gets the standard '\'' splice escape.
		{"embedded apostrophe", dir + `/it's.png`, `'` + dir + `/it'\''s.png'`},

		// Plain prose left alone.
		{"prose", `hello world`, `hello world`},

		// Multi-line paste left alone (typical code paste).
		{"multiline", "foo\nbar", "foo\nbar"},

		// Anything containing shell metacharacters is left alone so a
		// crafted "drop" can't smuggle a command.
		{"metachar", `/foo;rm -rf /`, `/foo;rm -rf /`},

		// Mixed: valid path token quoted, surrounding words preserved.
		{"path mixed with prose", dir + `/only.png is good`, `'` + dir + `/only.png' is good`},

		// URL path segment that happens to start with "/" but doesn't
		// exist on disk: must NOT be quoted or chip-collapsed. This
		// is the regression that motivated the existence check.
		{"url path segment", `/de/downloads/dokumentenarchiv`, `/de/downloads/dokumentenarchiv`},

		// Non-existent path that looks like a real one: also untouched.
		{"non-existent absolute", `/this/does/not/exist.png`, `/this/does/not/exist.png`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := quotePastedFilePaths(c.in)
			if got != c.want {
				t.Errorf("input %q\n  want %q\n  got  %q", c.in, c.want, got)
			}
		})
	}
}

// TestTildePathExists checks that ~ expansion in pathExists works
// when the home directory contains the candidate. We don't write
// into the user's real home; instead, override HOME for the test.
func TestTildePathExists(t *testing.T) {
	tmp := t.TempDir()
	makeFiles(t, tmp, "tilde-test.png")
	t.Setenv("HOME", tmp)

	in := `~/tilde-test.png`
	want := `'~/tilde-test.png'`
	got := quotePastedFilePaths(in)
	if got != want {
		t.Errorf("input %q\n  want %q\n  got  %q", in, want, got)
	}

	// A tilde path that doesn't exist must be left alone.
	in = `~/no-such-file.png`
	got = quotePastedFilePaths(in)
	if got != in {
		t.Errorf("non-existent tilde path mutated: got %q want %q", got, in)
	}
}
