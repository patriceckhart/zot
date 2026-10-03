// Command codemodevm is built to WebAssembly and embedded in zot.
package main

import (
	"fmt"
	"os"

	"github.com/patriceckhart/zot/packages/codemode/internal/vm/worker"
)

func main() {
	if err := worker.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "codemode worker:", err)
		os.Exit(1)
	}
}
