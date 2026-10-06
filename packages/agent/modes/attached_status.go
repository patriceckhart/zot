package modes

import "unicode/utf8"

// maxToolProgressView bounds streamed tool output kept for a live tool
// panel; the tail is shown.
const maxToolProgressView = 16 << 10

// tailBytes returns at most n trailing bytes of s, starting on a rune
// boundary.
func tailBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// SetExecutionStatus shows why attached host work is waiting (queued,
// awaiting approval, recovery blocked) next to the execution label. An
// empty status clears it. Safe to call from any goroutine.
func (i *Interactive) SetExecutionStatus(status string) {
	i.mu.Lock()
	changed := i.executionStatus != status
	i.executionStatus = status
	i.mu.Unlock()
	if changed {
		i.invalidate()
	}
}

// executionTag is the status bar tag for attached execution.
func (i *Interactive) executionTag() string {
	if i.cfg.ExecutionLabel == "" || i.executionStatus == "" {
		return i.cfg.ExecutionLabel
	}
	return i.cfg.ExecutionLabel + ", " + i.executionStatus
}
