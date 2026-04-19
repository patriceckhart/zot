// Tiny Go program that drives `zot rpc` as a subprocess.
//
// For an in-process Go embedding, use github.com/patriceckhart/zot/pkg/zotcore
// instead — this example exists to show what consumers in OTHER
// languages have to do.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: rpcdemo <prompt>")
		os.Exit(2)
	}
	prompt := strings.Join(os.Args[1:], " ")

	cmd := exec.Command("zot", "rpc")
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	must(err)
	stdout, err := cmd.StdoutPipe()
	must(err)
	must(cmd.Start())

	if tok := os.Getenv("ZOTCORE_RPC_TOKEN"); tok != "" {
		send(stdin, map[string]any{"id": "0", "type": "hello", "token": tok})
	}
	send(stdin, map[string]any{
		"id":      "1",
		"type":    "prompt",
		"message": prompt,
	})

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var ev map[string]any
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		switch ev["type"] {
		case "text_delta":
			fmt.Print(ev["delta"])
		case "tool_call":
			fmt.Fprintf(os.Stderr, "\n[tool] %v\n", ev["name"])
		case "error":
			fmt.Fprintf(os.Stderr, "\n[error] %v\n", ev["message"])
		case "done":
			fmt.Println()
			stdin.Close()
			_ = cmd.Wait()
			return
		}
	}
}

func send(w io.Writer, m map[string]any) {
	b, _ := json.Marshal(m)
	_, _ = w.Write(append(b, '\n'))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
