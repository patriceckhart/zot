// Package zotext is the Go SDK for writing zot extensions.
//
// An extension is a subprocess that talks to zot over its stdin/stdout
// in newline-delimited JSON. This package wraps the wire format so
// extension authors can write straightforward Go without reimplementing
// the protocol.
//
// Minimal example (registers /hello and replies with a static prompt):
//
//	package main
//
//	import "github.com/patriceckhart/zot/pkg/zotext"
//
//	func main() {
//	    ext := zotext.New("hello", "1.0.0")
//	    ext.Command("hello", "say hi", func(args string) zotext.Response {
//	        return zotext.Prompt("Greet me in three different languages.")
//	    })
//	    ext.Run()
//	}
//
// Build it, drop the binary + an extension.json next to it under
// `$ZOT_HOME/extensions/hello/`, and zot picks it up on next launch.
//
// The same wire format also has reference clients in TypeScript and
// Python under examples/extensions/. Use whichever language fits.
package zotext

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/patriceckhart/zot/internal/extproto"
)

// CommandHandler is invoked when the user runs the extension's
// registered slash command. args is everything the user typed after
// the command name (already trimmed). Return a Response describing
// what zot should do next.
type CommandHandler func(args string) Response

// Response tells zot how to react to a command invocation. Construct
// one with Prompt(), Insert(), Display(), or Noop().
type Response struct {
	Action  string // "prompt", "insert", "display", "noop"
	Prompt  string
	Insert  string
	Display string
	Error   string
}

// Prompt returns a Response that submits text as a fresh user message
// to the agent (running it through the model loop as if the user had
// typed and pressed enter).
func Prompt(text string) Response { return Response{Action: "prompt", Prompt: text} }

// Insert returns a Response that drops text into the editor at the
// cursor without submitting.
func Insert(text string) Response { return Response{Action: "insert", Insert: text} }

// Display returns a Response that adds a one-shot styled note to the
// chat without invoking the model. Useful for showing a result without
// burning tokens.
func Display(text string) Response { return Response{Action: "display", Display: text} }

// Noop returns a Response that signals "I handled it, no UI change".
// Use after pushing your own state or notifications.
func Noop() Response { return Response{Action: "noop"} }

// Errorf returns a Response that surfaces the error in the chat as a
// red status line.
func Errorf(format string, args ...any) Response {
	return Response{Action: "noop", Error: fmt.Sprintf(format, args...)}
}

// Extension is one zot extension. Construct with New, register
// commands, then call Run.
type Extension struct {
	name    string
	version string

	in      io.Reader
	out     io.Writer
	stderr  io.Writer
	writeMu sync.Mutex

	mu           sync.Mutex
	commands     map[string]CommandHandler
	descriptions []descTuple // ordered so register frames arrive in registration order

	// Caps reported in the hello frame. The SDK enables "commands"
	// automatically; future capabilities (tools, events) will live
	// here when those phases land.
	caps []string

	// hostInfo is filled in once HelloAck arrives.
	host HostInfo
}

type descTuple struct {
	name, desc string
}

// HostInfo is what the host (zot) tells us in HelloAck. Useful for
// extensions that want to behave differently per provider.
type HostInfo struct {
	ProtocolVersion int
	ZotVersion      string
	Provider        string
	Model           string
	CWD             string
}

// New constructs an Extension with the given identifier. name should
// match the name field in extension.json.
func New(name, version string) *Extension {
	return &Extension{
		name:     name,
		version:  version,
		in:       os.Stdin,
		out:      os.Stdout,
		stderr:   os.Stderr,
		commands: map[string]CommandHandler{},
		caps:     []string{"commands"},
	}
}

// Host returns the HostInfo received during the hello handshake.
// Returns the zero value if Run hasn't started yet.
func (e *Extension) Host() HostInfo { return e.host }

// Logf writes a line to the extension's stderr, which zot captures to
// $ZOT_HOME/logs/ext-<name>.log. Use this for debug output: anything
// you print to stdout would corrupt the JSON wire protocol.
func (e *Extension) Logf(format string, args ...any) {
	fmt.Fprintf(e.stderr, "["+e.name+"] "+format+"\n", args...)
}

// Command registers a slash-command handler. Call this BEFORE Run().
// Once Run is going, when the user runs /name in zot, fn is invoked
// with the remaining args.
//
// Naming conflicts with built-in commands (e.g. /help) are silently
// shadowed by the built-in; check the user's "ext logs" if a command
// you registered isn't taking effect.
func (e *Extension) Command(name, description string, fn CommandHandler) {
	e.mu.Lock()
	e.commands[name] = fn
	e.descriptions = append(e.descriptions, descTuple{name: name, desc: description})
	e.mu.Unlock()
}

// Notify pushes an info-level status note into zot's chat without
// requiring a slash command from the user.
func (e *Extension) Notify(level, message string) {
	_ = e.send(extproto.NotifyFromExt{
		Type:    "notify",
		Level:   level,
		Message: message,
	})
}

// Run starts the protocol loop. Blocks until stdin closes (zot has
// shut us down). Returns the first fatal error, or nil on clean exit.
func (e *Extension) Run() error {
	// Send hello, then re-announce all commands (covers Command calls
	// made before Run, and is also fine for those made after via
	// Command()'s direct send).
	if err := e.send(extproto.HelloFromExt{
		Type:         "hello",
		Name:         e.name,
		Version:      e.version,
		Capabilities: e.caps,
	}); err != nil {
		return err
	}
	e.mu.Lock()
	descs := append([]descTuple(nil), e.descriptions...)
	e.mu.Unlock()
	for _, d := range descs {
		_ = e.send(extproto.RegisterCommandFromExt{
			Type:        "register_command",
			Name:        d.name,
			Description: d.desc,
		})
	}

	scanner := bufio.NewScanner(e.in)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var frame extproto.Frame
		if err := json.Unmarshal(line, &frame); err != nil {
			e.Logf("malformed frame from host: %v", err)
			continue
		}
		switch frame.Type {
		case "hello_ack":
			var ack extproto.HelloAckFromHost
			if err := json.Unmarshal(line, &ack); err == nil {
				e.host = HostInfo{
					ProtocolVersion: ack.ProtocolVersion,
					ZotVersion:      ack.ZotVersion,
					Provider:        ack.Provider,
					Model:           ack.Model,
					CWD:             ack.CWD,
				}
			}
		case "command_invoked":
			var ci extproto.CommandInvokedFromHost
			if err := json.Unmarshal(line, &ci); err != nil {
				continue
			}
			e.mu.Lock()
			fn := e.commands[ci.Name]
			e.mu.Unlock()
			if fn == nil {
				e.respond(ci.ID, Errorf("no handler for /%s", ci.Name))
				continue
			}
			// Run the handler on its own goroutine so a slow handler
			// doesn't block subsequent commands. Order isn't promised.
			go func(id string, fn CommandHandler, args string) {
				defer func() {
					if r := recover(); r != nil {
						e.respond(id, Errorf("panic: %v", r))
					}
				}()
				resp := fn(args)
				e.respond(id, resp)
			}(ci.ID, fn, ci.Args)
		case "shutdown":
			_ = e.send(extproto.ShutdownAckFromExt{Type: "shutdown_ack"})
			return nil
		default:
			e.Logf("unknown frame type %q", frame.Type)
		}
	}
	return scanner.Err()
}

// respond serialises a CommandResponseFromExt for the given id.
func (e *Extension) respond(id string, r Response) {
	if r.Action == "" {
		r.Action = "noop"
	}
	_ = e.send(extproto.CommandResponseFromExt{
		Type:    "command_response",
		ID:      id,
		Action:  r.Action,
		Prompt:  r.Prompt,
		Insert:  r.Insert,
		Display: r.Display,
		Error:   r.Error,
	})
}

// send marshals v + LF and writes it under a mutex (so concurrent
// goroutines don't interleave bytes on stdout).
func (e *Extension) send(v any) error {
	b, err := extproto.Encode(v)
	if err != nil {
		return err
	}
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	_, err = e.out.Write(b)
	return err
}
