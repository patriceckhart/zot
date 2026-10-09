package tui

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/term"
)

// Terminal abstracts the real terminal for tests.
type Terminal interface {
	io.Writer
	// Size returns (cols, rows).
	Size() (int, int)
	// OnResize registers a callback invoked on SIGWINCH (best effort).
	OnResize(func())
	// EnterRaw puts the tty into raw mode. Returns a restore func.
	EnterRaw() (restore func() error, err error)
	// ReadByte reads one byte of input. Blocks.
	ReadByte() (byte, error)
	// PeekByteTimeout reads one byte of input but returns (0, false, nil)
	// if no byte arrives within the timeout. Used to disambiguate bare
	// Esc from the start of an escape sequence.
	PeekByteTimeout(time.Duration) (byte, bool, error)
	// SetNonblock sets stdin to non-blocking mode (used by paste handling).
	// May be a no-op on some platforms.
	SetNonblock(bool) error
}

// ProcTerm is a Terminal bound to the current process's tty.
type ProcTerm struct {
	out        *os.File
	in         *os.File
	resizeCBs  []func()
	resizeOnce sync.Once // Windows polls each terminal independently.
}

// NewProcTerm returns a Terminal bound to stdin/stdout.
func NewProcTerm() *ProcTerm {
	return &ProcTerm{out: os.Stdout, in: os.Stdin}
}

func (p *ProcTerm) Write(b []byte) (int, error) { return p.out.Write(b) }

func (p *ProcTerm) Size() (int, int) {
	w, h, err := term.GetSize(int(p.out.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return 80, 24
	}
	return w, h
}

func (p *ProcTerm) OnResize(fn func()) {
	p.resizeCBs = append(p.resizeCBs, fn)
	p.installResizeHandler()
}

func (p *ProcTerm) EnterRaw() (func() error, error) {
	fd := int(p.in.Fd())
	if !term.IsTerminal(fd) {
		return func() error { return nil }, nil
	}
	st, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() error { return term.Restore(fd, st) }, nil
}

func (p *ProcTerm) ReadByte() (byte, error) {
	var b [1]byte
	_, err := p.in.Read(b[:])
	return b[0], err
}

// PeekByteTimeout uses a platform-specific non-blocking read to decide
// whether another byte is available within d. If not, returns (0, false, nil).
func (p *ProcTerm) PeekByteTimeout(d time.Duration) (byte, bool, error) {
	return peekStdin(p.in, d)
}

// ---- terminal control sequences ----

// HideCursor, ShowCursor, ClearScreen, BracketedPasteOn/Off, etc.
const (
	SeqHideCursor  = "\x1b[?25l"
	SeqShowCursor  = "\x1b[?25h"
	SeqClearScreen = "\x1b[2J\x1b[H"
	// SeqClearScreenNoHome is SeqClearScreen without the trailing
	// cursor-home (\x1b[H). Use it whenever a MoveTo(...) follows
	// immediately. VS Code's integrated terminal interprets a bare
	// CUP-no-args during a clear as "snap the viewport scrollbar to
	// row 0", which makes the user's scroll position jump on every
	// repaint. The explicit MoveTo we emit afterwards positions the
	// cursor identically without triggering that snap.
	SeqClearScreenNoHome = "\x1b[2J"
	SeqClearScrollback   = "\x1b[3J"
	// SeqCursorHome moves to the top-left of the visible viewport.
	// SeqClearToEnd erases from the cursor to the end of the screen
	// without scrolling content into scrollback (unlike \x1b[2J on
	// xterm.js). Together they clear the visible frame in place, which
	// the VS Code terminal needs for duplicate-free full repaints.
	SeqCursorHome        = "\x1b[H"
	SeqClearToEnd        = "\x1b[0J"
	SeqClearLine         = "\x1b[2K"
	SeqResetScrollRegion = "\x1b[r"
	SeqDeleteKittyImages = "\x1b_Ga=d\x1b\\"
	SeqBracketedPasteOn  = "\x1b[?2004h"
	SeqBracketedPasteOff = "\x1b[?2004l"
	// Request enhanced keyboard reporting where supported. Kitty-style
	// keyboard protocol covers Ghostty, Kitty, VS Code's integrated
	// terminal, and recent xterm.js builds. Xterm modifyOtherKeys is a
	// useful fallback for terminals/tmux configurations that expose
	// modified Enter as CSI 27;<mod>;<code>~.
	SeqEnhancedKeyboardOn  = "\x1b[>1u\x1b[>4;2m"
	SeqEnhancedKeyboardOff = "\x1b[<u\x1b[>4m"
	// Button-motion tracking with SGR coordinates supports clicks and drags.
	// Enabled only by the interactive click-to-position setting.
	SeqMouseOn         = "\x1b[?1000h\x1b[?1002h\x1b[?1006h"
	SeqMouseOff        = "\x1b[?1000l\x1b[?1002l\x1b[?1006l"
	SeqAltScreenOn     = "\x1b[?1049h"
	SeqAltScreenOff    = "\x1b[?1049l"
	SeqSynchronizedOn  = "\x1b[?2026h"
	SeqSynchronizedOff = "\x1b[?2026l"
	// SeqSaveCursor / SeqRestoreCursor use DECSC/DECRC. All terminals
	// we target adjust the saved row when natural scrolling occurs, so
	// these survive scroll-on-write inside the bottom band.
	SeqSaveCursor    = "\x1b7"
	SeqRestoreCursor = "\x1b8"
	// SeqEraseToEnd erases from the cursor to the end of the screen.
	SeqEraseToEnd = "\x1b[J"
)

// ReportCWD returns the OSC 7 sequence that tells the terminal the
// current working directory, formatted as a file URL. Terminals such
// as kitty, WezTerm, iTerm2, and GNOME Terminal use this to open new
// tabs / splits in the same directory. Returns "" for an empty path so
// callers can write it unconditionally. The path is percent-encoded
// per RFC 3986 (only unreserved characters and the path separator are
// left bare) and prefixed with the local hostname.
func ReportCWD(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	host, _ := os.Hostname()
	return "\x1b]7;file://" + host + osc7EscapePath(abs) + "\x07"
}

// osc7EscapePath percent-encodes a filesystem path for an OSC 7 file
// URL. Forward slashes (the URL path separator) and the unreserved set
// (A-Z a-z 0-9 - _ . ~) pass through; everything else is %XX-encoded.
// On Windows the backslash separators are normalised to forward
// slashes and a leading slash is added so "C:\foo" becomes "/C:/foo".
func osc7EscapePath(p string) string {
	p = filepath.ToSlash(p)
	if len(p) > 1 && p[1] == ':' {
		p = "/" + p
	}
	const hex = "0123456789ABCDEF"
	var b []byte
	for i := 0; i < len(p); i++ {
		c := p[i]
		unreserved := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' || c == '~' || c == '/'
		if unreserved {
			b = append(b, c)
			continue
		}
		b = append(b, '%', hex[c>>4], hex[c&0x0f])
	}
	return string(b)
}

// SetScrollRegion returns the DECSTBM sequence to set the terminal's
// scroll region (1-indexed, inclusive). Lines that scroll out of this
// region still flow into the terminal's scrollback, but content above
// or below the region is not affected by scroll-induced movement.
func SetScrollRegion(top, bottom int) string {
	return "\x1b[" + itoa(top) + ";" + itoa(bottom) + "r"
}

// MoveTo moves the cursor to 1-indexed (row, col).
func MoveTo(row, col int) string {
	return "\x1b[" + itoa(row) + ";" + itoa(col) + "H"
}
