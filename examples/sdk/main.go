// In-process Go embedding of the zot agent runtime via the zotcore SDK.
// Compare to examples/rpc/go which spawns `zot rpc` as a subprocess.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/patriceckhart/zot/pkg/zotcore"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: sdkdemo <prompt>")
		os.Exit(2)
	}
	prompt := strings.Join(os.Args[1:], " ")

	rt, err := zotcore.New(zotcore.Config{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer rt.Close()

	events, err := rt.Prompt(context.Background(), prompt, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for ev := range events {
		switch ev.Type {
		case "text_delta":
			fmt.Print(ev.Delta)
		case "tool_call":
			fmt.Fprintf(os.Stderr, "\n[tool] %s\n", ev.Name)
		case "error":
			fmt.Fprintf(os.Stderr, "\n[error] %s\n", ev.Error)
		}
	}
	fmt.Println()
	fmt.Fprintf(os.Stderr, "cost so far: $%.4f\n", rt.Cost().CostUSD)
}
