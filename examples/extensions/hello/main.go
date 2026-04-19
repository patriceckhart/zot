// hello — a tiny zot extension that registers /hello and /summon.
//
// Build it:
//
//	cd examples/extensions/hello
//	go build -o hello .
//
// Then drop it next to its extension.json under
// $ZOT_HOME/extensions/hello/, or run `zot ext install ./hello`
// from this directory.
package main

import (
	"strings"

	"github.com/patriceckhart/zot/pkg/zotext"
)

func main() {
	ext := zotext.New("hello", "1.0.0")

	// /hello [name] — submits a friendly prompt to the agent.
	ext.Command("hello", "say hello (optional name)", func(args string) zotext.Response {
		who := strings.TrimSpace(args)
		if who == "" {
			return zotext.Prompt("Greet me with a short, slightly absurd compliment.")
		}
		return zotext.Prompt("Greet " + who + " with a short, slightly absurd compliment.")
	})

	// /summon — pushes a notice into the chat without involving the
	// model. Useful for pretending we did something important.
	ext.Command("summon", "show a tongue-in-cheek summon notice", func(args string) zotext.Response {
		ext.Notify("info", "the daemon stirs in its cage.")
		return zotext.Display("a wisp of incense curls past your terminal.")
	})

	if err := ext.Run(); err != nil {
		ext.Logf("fatal: %v", err)
	}
}
