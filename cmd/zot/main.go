// Command zot is a lightweight terminal coding agent.
package main

import (
	"fmt"
	"os"

	"github.com/patriceckhart/zot/internal/agent"
)

// Injected at build time via -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
// See .goreleaser.yaml for the release build and the Makefile for
// local builds. Defaults make `zot --version` print something sensible
// when built without ldflags.
var (
	// 0.0.0 is the pre-release placeholder for local / untagged
	// builds. The first published GitHub release will be tagged
	// v0.0.1; everything before that ships as 0.0.0 from source.
	version = "0.0.0"
	commit  = ""
	date    = ""
)

func main() {
	v := version
	if commit != "" {
		short := commit
		if len(short) > 7 {
			short = short[:7]
		}
		v = v + " (" + short
		if date != "" {
			v = v + ", " + date
		}
		v = v + ")"
	}
	if err := agent.Run(os.Args[1:], v); err != nil {
		fmt.Fprintln(os.Stderr, "zot:", err)
		os.Exit(1)
	}
}
