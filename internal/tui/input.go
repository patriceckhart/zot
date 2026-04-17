package tui

import (
	"strings"
	"time"
)

// Key is a parsed keypress.
type Key struct {
	Kind  KeyKind
	Rune  rune   // for KeyRune
	Paste string // for KeyPaste
	Ctrl  bool
	Alt   bool
}

type KeyKind int

const (
	KeyRune KeyKind = iota
	KeyEnter
	KeyBackspace
	KeyTab
	KeyShiftTab
	KeyEsc
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyDelete
	KeyCtrlC
	KeyCtrlD
	KeyCtrlL
	KeyCtrlU
	KeyCtrlK
	KeyCtrlA
	KeyCtrlE
	KeyCtrlW
	KeyPaste
	KeyUnknown
)

// Reader parses a byte stream into Key events. It understands basic
// xterm escape sequences and bracketed paste.
type Reader struct {
	src  func() (byte, error)
	peek func(time.Duration) (byte, bool, error) // optional; may be nil
}

// NewReader returns a Reader that pulls bytes from read.
func NewReader(read func() (byte, error)) *Reader { return &Reader{src: read} }

// NewReaderWithPeek returns a Reader that pulls bytes from read and uses
// peek to disambiguate bare Esc from the start of an escape sequence.
func NewReaderWithPeek(read func() (byte, error), peek func(time.Duration) (byte, bool, error)) *Reader {
	return &Reader{src: read, peek: peek}
}

// Read returns the next parsed Key.
func (r *Reader) Read() (Key, error) {
	b, err := r.src()
	if err != nil {
		return Key{}, err
	}
	switch {
	case b == 0x03:
		return Key{Kind: KeyCtrlC}, nil
	case b == 0x04:
		return Key{Kind: KeyCtrlD}, nil
	case b == 0x0c:
		return Key{Kind: KeyCtrlL}, nil
	case b == 0x15:
		return Key{Kind: KeyCtrlU}, nil
	case b == 0x0b:
		return Key{Kind: KeyCtrlK}, nil
	case b == 0x01:
		return Key{Kind: KeyCtrlA}, nil
	case b == 0x05:
		return Key{Kind: KeyCtrlE}, nil
	case b == 0x17:
		return Key{Kind: KeyCtrlW}, nil
	case b == '\r', b == '\n':
		return Key{Kind: KeyEnter}, nil
	case b == '\t':
		return Key{Kind: KeyTab}, nil
	case b == 0x7f, b == 0x08:
		return Key{Kind: KeyBackspace}, nil
	case b == 0x1b:
		return r.readEscape()
	case b < 0x20:
		return Key{Kind: KeyUnknown}, nil
	}
	// UTF-8 multibyte?
	if b < 0x80 {
		return Key{Kind: KeyRune, Rune: rune(b)}, nil
	}
	// Decode UTF-8 (up to 4 bytes).
	n := utf8Len(b)
	buf := []byte{b}
	for i := 1; i < n; i++ {
		bb, err := r.src()
		if err != nil {
			return Key{}, err
		}
		buf = append(buf, bb)
	}
	rn, _ := decodeRune(buf)
	return Key{Kind: KeyRune, Rune: rn}, nil
}

func utf8Len(b byte) int {
	switch {
	case b&0xe0 == 0xc0:
		return 2
	case b&0xf0 == 0xe0:
		return 3
	case b&0xf8 == 0xf0:
		return 4
	}
	return 1
}

func decodeRune(b []byte) (rune, int) {
	// Minimal decoder; invalid runes become U+FFFD.
	if len(b) == 1 {
		return rune(b[0]), 1
	}
	var r rune
	switch len(b) {
	case 2:
		r = rune(b[0]&0x1f)<<6 | rune(b[1]&0x3f)
	case 3:
		r = rune(b[0]&0x0f)<<12 | rune(b[1]&0x3f)<<6 | rune(b[2]&0x3f)
	case 4:
		r = rune(b[0]&0x07)<<18 | rune(b[1]&0x3f)<<12 | rune(b[2]&0x3f)<<6 | rune(b[3]&0x3f)
	default:
		r = 0xFFFD
	}
	return r, len(b)
}

// readEscape handles sequences starting with 0x1b.
func (r *Reader) readEscape() (Key, error) {
	// Bare ESC: maybe followed by another byte within a short window.
	b, have, err := r.readEscapeNext(50 * time.Millisecond)
	if err != nil || !have {
		return Key{Kind: KeyEsc}, nil
	}
	switch b {
	case '[':
		return r.readCSI()
	case 'O':
		// SS3 sequences (function keys in some terminals).
		c, err := r.src()
		if err != nil {
			return Key{}, err
		}
		switch c {
		case 'H':
			return Key{Kind: KeyHome}, nil
		case 'F':
			return Key{Kind: KeyEnd}, nil
		}
		return Key{Kind: KeyUnknown}, nil
	case 0x7f, 0x08:
		// Alt+Backspace (Option+Delete on macOS) — most terminals send
		// ESC + DEL for this. Surface as a dedicated "alt backspace"
		// so the editor can map it to delete-word.
		return Key{Kind: KeyBackspace, Alt: true}, nil
	case 'b':
		// Emacs-style word-left, also emitted by some terminals for
		// Option+LeftArrow.
		return Key{Kind: KeyLeft, Alt: true}, nil
	case 'f':
		// Emacs-style word-right, also emitted for Option+RightArrow.
		return Key{Kind: KeyRight, Alt: true}, nil
	default:
		// Alt+<char>
		if b < 0x80 {
			return Key{Kind: KeyRune, Rune: rune(b), Alt: true}, nil
		}
	}
	return Key{Kind: KeyUnknown}, nil
}

// readEscapeNext tries to read one byte within d. If peek is available
// we use it (true non-blocking). Otherwise we fall back to a blocking
// read, which means bare Esc is only detected after the next keystroke.
func (r *Reader) readEscapeNext(d time.Duration) (byte, bool, error) {
	if r.peek != nil {
		return r.peek(d)
	}
	b, err := r.src()
	if err != nil {
		return 0, false, err
	}
	return b, true, nil
}

// readCSI parses a CSI sequence after ESC [.
func (r *Reader) readCSI() (Key, error) {
	var params []byte
	for {
		c, err := r.src()
		if err != nil {
			return Key{}, err
		}
		if c >= 0x30 && c <= 0x3f {
			params = append(params, c)
			continue
		}
		// Final byte.
		return r.dispatchCSI(string(params), c), nil
	}
}

func (r *Reader) dispatchCSI(params string, final byte) Key {
	// Modified arrow keys come in as CSI 1;<mod><final>. Modifier values
	// we care about: 2=Shift, 3=Alt/Option, 5=Ctrl. We only extract Alt.
	var alt bool
	if params != "" {
		if i := strings.IndexByte(params, ';'); i >= 0 {
			mod := params[i+1:]
			if mod == "3" || mod == "4" || mod == "7" || mod == "8" {
				// 3=Alt, 4=Shift+Alt, 7=Ctrl+Alt, 8=Ctrl+Shift+Alt
				alt = true
			}
		}
	}
	switch final {
	case 'A':
		return Key{Kind: KeyUp, Alt: alt}
	case 'B':
		return Key{Kind: KeyDown, Alt: alt}
	case 'C':
		return Key{Kind: KeyRight, Alt: alt}
	case 'D':
		return Key{Kind: KeyLeft, Alt: alt}
	case 'H':
		return Key{Kind: KeyHome}
	case 'F':
		return Key{Kind: KeyEnd}
	case 'Z':
		return Key{Kind: KeyShiftTab}
	case '~':
		switch params {
		case "3":
			return Key{Kind: KeyDelete}
		case "5":
			return Key{Kind: KeyPageUp}
		case "6":
			return Key{Kind: KeyPageDown}
		case "200":
			// Start of bracketed paste.
			return r.readPaste()
		}
	}
	return Key{Kind: KeyUnknown}
}

// readPaste reads until ESC [ 2 0 1 ~ and returns the pasted text.
func (r *Reader) readPaste() Key {
	var sb strings.Builder
	const end = "\x1b[201~"
	tail := make([]byte, 0, len(end))
	for {
		b, err := r.src()
		if err != nil {
			break
		}
		tail = append(tail, b)
		if len(tail) > len(end) {
			sb.WriteByte(tail[0])
			tail = tail[1:]
		}
		if string(tail) == end {
			break
		}
	}
	return Key{Kind: KeyPaste, Paste: sb.String()}
}
