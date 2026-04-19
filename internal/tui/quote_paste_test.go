package tui

import "testing"

func TestQuotePastedFilePaths(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// macOS Terminal default: backslash-escaped spaces in path.
		{"backslash-escaped space", `/Users/pat/foo\ bar.png`, `'/Users/pat/foo bar.png'`},

		// Plain absolute path with no special chars.
		{"plain absolute", `/Users/pat/file.png`, `'/Users/pat/file.png'`},

		// file:// URL form; URL-decoded and scheme stripped.
		{"file url", `file:///Users/pat/foo%20bar.png`, `'/Users/pat/foo bar.png'`},

		// Tilde paths.
		{"tilde", `~/Pictures/x.png`, `'~/Pictures/x.png'`},

		// Multi-file drop, each path quoted independently.
		{"two paths", `/a.png /b.png`, `'/a.png' '/b.png'`},

		// Already single-quoted: re-normalise to consistent quoting.
		{"already single quoted", `'/Users/pat/x.png'`, `'/Users/pat/x.png'`},

		// Already double-quoted: re-normalise.
		{"already double quoted", `"/Users/pat/x.png"`, `'/Users/pat/x.png'`},

		// Embedded apostrophe gets the standard '\'' splice escape.
		{"embedded apostrophe", `/Users/pat/it's.png`, `'/Users/pat/it'\''s.png'`},

		// Plain prose left alone.
		{"prose", `hello world`, `hello world`},

		// Multi-line paste left alone (typical code paste).
		{"multiline", "foo\nbar", "foo\nbar"},

		// Anything containing shell metacharacters is left alone so a
		// crafted "drop" can't smuggle a command.
		{"metachar", `/foo;rm -rf /`, `/foo;rm -rf /`},

		// Mixed: valid path token quoted, surrounding words preserved.
		{"path mixed with prose", `/x.png is good`, `'/x.png' is good`},
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
