// Command zot is a lightweight terminal coding agent.
package main

import (
	"fmt"
	"os"

	"github.com/patriceckhart/zot/internal/agent"
)

var version = "dev"

func main() {
	if err := agent.Run(os.Args[1:], version); err != nil {
		fmt.Fprintln(os.Stderr, "zot:", err)
		os.Exit(1)
	}
}
