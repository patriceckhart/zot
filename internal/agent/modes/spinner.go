package modes

import (
	"math/rand"
	"time"
)

// spinner drives the busy animation shown in the status bar while a
// turn is streaming. It rotates through a list of playful status
// messages and a small frame animation.
type spinner struct {
	frames    []string
	messages  []string
	startedAt time.Time
	msgIdx    int
	lastSwap  time.Time

	// fixedMsg overrides the rotating funnyWorkingLines message when
	// set. Used for auto-compaction so the spinner clearly says what's
	// happening instead of cycling jokes.
	fixedMsg string
}

// funnyWorkingLines is the rotating text. Kept deliberately short so it
// fits next to the token counter on narrow terminals. A handful of
// craftsman-style aphorisms are folded in among the irreverent lines
// for a bit of contrast — the rotation never feels too uniform.
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
	"every draft is a stone in the work",
	"bringing order to the unhewn",
	"finding the load-bearing measure",
	"where clarity grows, work grows lighter",
	"every correction serves the work",
}

// spinnerFrames is the cli-spinners "dots3" preset — a 10-frame
// single-cell braille spinner that reads as a small box of dots
// rotating around its perimeter. Source:
// https://github.com/sindresorhus/cli-spinners (MIT). Used at the
// preset's recommended 80ms per step.
var spinnerFrames = []string{
	"⠋",
	"⠙",
	"⠚",
	"⠞",
	"⠖",
	"⠦",
	"⠴",
	"⠲",
	"⠳",
	"⠓",
}

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
	s.fixedMsg = ""
}

// StartFixed is like Start but pins the status text to msg for the
// duration of this spinner run. Cleared by the next Start() call.
func (s *spinner) StartFixed(msg string) {
	s.startedAt = time.Now()
	s.lastSwap = s.startedAt
	s.fixedMsg = msg
}

// Frame returns the current spinner glyph for the running animation.
//
// 80ms per step matches the dots3 preset's recommended interval.
func (s *spinner) Frame() string {
	if s.startedAt.IsZero() {
		return s.frames[0]
	}
	elapsed := time.Since(s.startedAt)
	idx := int(elapsed/(80*time.Millisecond)) % len(s.frames)
	return s.frames[idx]
}

// Message returns the current rotating status text. The text changes
// every ~2.5 seconds so the spinner doesn't look frozen. When the
// spinner was started via StartFixed, the pinned message is returned
// unchanged.
func (s *spinner) Message() string {
	if s.fixedMsg != "" {
		return s.fixedMsg
	}
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
