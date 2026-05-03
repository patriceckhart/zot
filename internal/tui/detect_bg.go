package tui

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// DetectThemeFromBackground queries the controlling tty for its
// current background colour using the OSC 11 escape sequence and
// returns Dark or Light based on the response's perceived
// luminance. Falls back to Dark when the terminal does not
// reply within timeout, which is the expected behaviour for
// terminals that do not implement OSC 11 (Linux console, VS Code's
// integrated terminal in some configurations, tmux without
// pass-through, very old emulators).
//
// The query / parse runs synchronously before the TUI is
// initialised so the returned theme can drive the entire session.
// We briefly put stdin into raw mode and disable echo so the OSC
// reply doesn't leak onto the user's screen as visible bytes.
func DetectThemeFromBackground(timeout time.Duration) Theme {
	// Honour explicit override env var first; some users / CI envs
	// know better than the heuristic.
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ZOT_THEME"))) {
	case "dark":
		return Dark
	case "light":
		return Light
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return Dark
	}

	fd := int(os.Stdin.Fd())
	st, err := term.MakeRaw(fd)
	if err != nil {
		return Dark
	}
	defer term.Restore(fd, st)

	// Send the query. ST (\x1b\\) and BEL (\x07) are both accepted
	// terminators; some terminals only honour one of them, so we
	// send BEL which is more widely supported.
	if _, err := os.Stdout.Write([]byte("\x1b]11;?\x07")); err != nil {
		return Dark
	}

	deadline := time.Now().Add(timeout)
	resp := readOSCResponse(deadline)
	if resp == "" {
		return Dark
	}

	r, g, b, ok := parseOSC11Reply(resp)
	if !ok {
		return Dark
	}

	// Rec. 709 luma. Threshold at 0.5: anything brighter than
	// mid-grey gets the light theme.
	luma := 0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)
	if luma >= 0.5 {
		return Light
	}
	return Dark
}

// readOSCResponse drains stdin into a small buffer until either a
// terminator (BEL or ST) is seen, the deadline expires, or stdin
// hits EOF. Returns whatever was collected, or "" on no usable
// response.
func readOSCResponse(deadline time.Time) string {
	var buf [128]byte
	n := 0
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		b, ok, err := peekStdin(os.Stdin, remaining)
		if err != nil || !ok {
			return string(buf[:n])
		}
		if n < len(buf) {
			buf[n] = b
			n++
		}
		// BEL terminator
		if b == 0x07 {
			return string(buf[:n])
		}
		// ST terminator: ESC then '\\'. We saw it the moment the
		// previous byte was ESC and the current byte is '\\'.
		if n >= 2 && buf[n-2] == 0x1b && buf[n-1] == '\\' {
			return string(buf[:n])
		}
	}
	return string(buf[:n])
}

// parseOSC11Reply extracts the (r, g, b) colour components from an
// OSC 11 reply of the form "\x1b]11;rgb:RRRR/GGGG/BBBB\x07" (or with
// ST terminator). The component widths can be 1, 2, 3, or 4 hex
// digits per channel; we normalise them into the 0..1 range.
func parseOSC11Reply(s string) (float64, float64, float64, bool) {
	// Locate "rgb:" within the response.
	i := strings.Index(s, "rgb:")
	if i < 0 {
		return 0, 0, 0, false
	}
	body := s[i+len("rgb:"):]
	// Trim trailing terminator(s).
	body = strings.TrimRight(body, "\x07")
	if strings.HasSuffix(body, "\x1b\\") {
		body = strings.TrimSuffix(body, "\x1b\\")
	}
	parts := strings.Split(body, "/")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	parse := func(hexstr string) (float64, bool) {
		if len(hexstr) == 0 || len(hexstr) > 4 {
			return 0, false
		}
		v, err := strconv.ParseUint(hexstr, 16, 32)
		if err != nil {
			return 0, false
		}
		// Normalise to 0..1: max value is (16^len - 1).
		max := uint64(1)
		for j := 0; j < len(hexstr); j++ {
			max *= 16
		}
		max--
		if max == 0 {
			return 0, false
		}
		return float64(v) / float64(max), true
	}
	r, ok1 := parse(parts[0])
	g, ok2 := parse(parts[1])
	b, ok3 := parse(parts[2])
	if !ok1 || !ok2 || !ok3 {
		return 0, 0, 0, false
	}
	return r, g, b, true
}

// debugDetect is used only in development to help diagnose
// detection issues; never invoked in production code paths.
func debugDetect() {
	fmt.Fprintf(os.Stderr, "zot theme detection: stdin tty=%v stdout tty=%v\n",
		term.IsTerminal(int(os.Stdin.Fd())),
		term.IsTerminal(int(os.Stdout.Fd())))
}
