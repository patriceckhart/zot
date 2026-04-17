package modes

import (
	"math/rand"
	"time"
)

// spinner drives the busy animation shown in the status bar while a
// turn is streaming. It rotates through a list of playful status
// messages and a small frame animation.
type spinner struct {
	frames   []string
	messages []string
	startedAt time.Time
	msgIdx   int
	lastSwap time.Time
}

// funnyWorkingLines is the rotating text. Kept deliberately short so it
// fits next to the token counter on narrow terminals.
var funnyWorkingLines = []string{
	"thinking",
	"reticulating splines",
	"bribing the tokenizer",
	"asking the rubber duck",
	"summoning daemons",
	"consulting the oracle",
	"herding tokens",
	"compiling excuses",
	"poking the model",
	"negotiating with rate limits",
	"picking a fight with syntax",
	"reading between the bits",
	"tasting the semicolons",
	"pretending to understand go generics",
	"petting the cache",
	"drafting clever replies",
	"warming up the GPU choir",
	"arguing with a stack trace",
	"googling the answer (not really)",
	"rewriting history",
}

// spinnerFrames is a smooth braille spinner.
var spinnerFrames = []string{"\u280b", "\u2819", "\u2839", "\u2838", "\u283c", "\u2834", "\u2826", "\u2827", "\u2807", "\u280f"}

// newSpinner constructs a fresh spinner.
func newSpinner() *spinner {
	s := &spinner{
		frames:   spinnerFrames,
		messages: funnyWorkingLines,
	}
	return s
}

// Start resets the spinner to the beginning of its animation and picks
// a random opening message.
func (s *spinner) Start() {
	s.startedAt = time.Now()
	s.msgIdx = rand.Intn(len(s.messages))
	s.lastSwap = s.startedAt
}

// Frame returns the current spinner glyph for the running animation.
func (s *spinner) Frame() string {
	if s.startedAt.IsZero() {
		return s.frames[0]
	}
	elapsed := time.Since(s.startedAt)
	// 120ms per frame matches the interactive tick rate.
	idx := int(elapsed/(120*time.Millisecond)) % len(s.frames)
	return s.frames[idx]
}

// Message returns the current rotating status text. The text changes
// every ~2.5 seconds so the spinner doesn't look frozen.
func (s *spinner) Message() string {
	if time.Since(s.lastSwap) > 2500*time.Millisecond {
		s.msgIdx = (s.msgIdx + 1) % len(s.messages)
		s.lastSwap = time.Now()
	}
	return s.messages[s.msgIdx]
}

// Elapsed returns the wall-clock duration the spinner has been running.
func (s *spinner) Elapsed() time.Duration {
	if s.startedAt.IsZero() {
		return 0
	}
	return time.Since(s.startedAt).Round(time.Second)
}
