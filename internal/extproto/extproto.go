// Package extproto defines the JSON-over-stdin/stdout wire format
// spoken between zot and its extension subprocesses. Both the host
// (internal/agent/extensions) and the SDK (pkg/zotext) marshal/
// unmarshal the same types, so changes here ripple through both.
//
// All frames are one JSON object terminated by a single LF. Object
// boundaries follow newline boundaries; no multi-line JSON.
//
// Direction conventions in this file:
//   - Type names ending in "FromExt" are sent by the extension to zot.
//   - Type names ending in "FromHost" are sent by zot to the extension.
//   - Names without a suffix are direction-neutral payloads or shared
//     value types.
//
// Every frame has a top-level Type discriminator. Optional ID is
// present on commands and on responses to commands so the sender can
// correlate; events and notifications never carry an ID.
package extproto

import "encoding/json"

// ProtocolVersion is the major version of this wire format. Bumped
// for breaking changes; minor additions don't bump.
const ProtocolVersion = 1

// Frame is the lowest-common-denominator parse target so a reader can
// peek at the type before unmarshalling the full payload.
type Frame struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

// ---- extension -> host ----

// HelloFromExt is the first frame the extension sends after start.
// Zot replies with HelloAckFromHost, then registration frames
// (RegisterCommandFromExt, etc.) flow.
type HelloFromExt struct {
	Type         string   `json:"type"` // "hello"
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// RegisterCommandFromExt asks zot to bind /name to this extension.
// Description appears in the slash autocomplete + /help.
type RegisterCommandFromExt struct {
	Type        string `json:"type"` // "register_command"
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// RegisterToolFromExt asks zot to expose a tool to the LLM. The
// schema is a JSON Schema object describing Args; zot doesn't validate
// the model's arguments against it (the model providers do that), but
// it must parse as valid JSON or registration is rejected.
//
// Tool names live in the same namespace as built-in tools (read,
// write, edit, bash, skill). Conflicts are silently shadowed by the
// built-in; check the extension's log for a warning.
type RegisterToolFromExt struct {
	Type        string          `json:"type"` // "register_tool"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
}

// ReadyFromExt signals "all initial registrations sent". The host
// waits for this (with a short timeout) before building the agent's
// tool registry, so model calls don't race extension tool
// registration.
type ReadyFromExt struct {
	Type string `json:"type"` // "ready"
}

// ToolResultFromExt is the extension's reply to a ToolCallFromHost.
// Content[] follows the same shape as elsewhere in zot:
//
//	{"type":"text", "text":"..."}
//	{"type":"image", "mime_type":"image/png", "data":"<base64>"}
//
// Set IsError true to mark the tool call as failed; the model sees
// the content as the error explanation.
type ToolResultFromExt struct {
	Type    string         `json:"type"` // "tool_result"
	ID      string         `json:"id"`
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"is_error,omitempty"`
}

// ContentBlock is one entry in a tool result's content array.
type ContentBlock struct {
	Type     string `json:"type"` // "text" | "image"
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"` // base64
}

// CommandResponseFromExt is the extension's answer to a
// CommandInvokedFromHost. Action drives what zot does next:
//
//   - "prompt"  → submit Prompt as a fresh user message to the agent
//   - "insert"  → insert Insert into the editor buffer at the cursor
//   - "display" → append Display to the chat as a one-shot note
//     (no model call, no transcript entry)
//   - "noop"    → command handled internally, no UI change
type CommandResponseFromExt struct {
	Type    string `json:"type"` // "command_response"
	ID      string `json:"id"`
	Action  string `json:"action"`            // see above
	Prompt  string `json:"prompt,omitempty"`  // for action=prompt
	Insert  string `json:"insert,omitempty"`  // for action=insert
	Display string `json:"display,omitempty"` // for action=display
	Error   string `json:"error,omitempty"`   // command failed; render to user
}

// NotifyFromExt is a one-way status message the extension can push at
// any time. Zot renders it in the chat as a styled note.
type NotifyFromExt struct {
	Type    string `json:"type"`  // "notify"
	Level   string `json:"level"` // "info" | "warn" | "error" | "success"
	Message string `json:"message"`
}

// ShutdownAckFromExt acknowledges the host's shutdown request. The
// extension should exit shortly after sending this; zot waits a few
// seconds before SIGTERM.
type ShutdownAckFromExt struct {
	Type string `json:"type"` // "shutdown_ack"
}

// ---- host -> extension ----

// HelloAckFromHost is zot's reply to HelloFromExt. The extension may
// inspect the host version + currently-active provider/model to decide
// whether to register particular commands.
type HelloAckFromHost struct {
	Type            string `json:"type"` // "hello_ack"
	ProtocolVersion int    `json:"protocol_version"`
	ZotVersion      string `json:"zot_version"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	CWD             string `json:"cwd"`
}

// CommandInvokedFromHost is sent when the user runs a slash command
// the extension previously registered. Args contains everything after
// the command name (already trimmed).
type CommandInvokedFromHost struct {
	Type string `json:"type"` // "command_invoked"
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

// ToolCallFromHost is sent when the LLM invokes a tool the extension
// registered. Args is the raw JSON object the model produced; the
// extension is responsible for validating/coercing it. Reply with
// ToolResultFromExt within the host's tool timeout (default 60s).
type ToolCallFromHost struct {
	Type string          `json:"type"` // "tool_call"
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

// ShutdownFromHost asks the extension to clean up and exit. Zot
// sends this when the user runs /reload-ext or zot itself is exiting
// gracefully. Extensions that don't reply within a few seconds get
// SIGTERM; SIGKILL after a few more.
type ShutdownFromHost struct {
	Type string `json:"type"` // "shutdown"
}

// ---- error frame (either direction) ----

// Error is a generic failure response. Used by either side when a
// frame can't be processed (malformed JSON, unknown type, etc.).
type Error struct {
	Type    string `json:"type"` // "error"
	ID      string `json:"id,omitempty"`
	Message string `json:"message"`
}

// ---- helpers ----

// Encode marshals v and appends a trailing LF, ready to write to the
// peer's pipe. Returns the marshalling error, if any.
func Encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
