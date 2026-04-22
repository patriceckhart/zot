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

const ProtocolVersion = 1

type Frame struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
}

type HelloFromExt struct {
	Type         string   `json:"type"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type RegisterCommandFromExt struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type RegisterToolFromExt struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema"`
}

type ReadyFromExt struct {
	Type string `json:"type"`
}

type SubscribeFromExt struct {
	Type      string   `json:"type"`
	Events    []string `json:"events,omitempty"`
	Intercept []string `json:"intercept,omitempty"`
}

type EventInterceptResponseFromExt struct {
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	Block        bool            `json:"block,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	ModifiedArgs json.RawMessage `json:"modified_args,omitempty"`
	ReplaceText  string          `json:"replace_text,omitempty"`
}

type ToolResultFromExt struct {
	Type    string         `json:"type"`
	ID      string         `json:"id"`
	Content []ContentBlock `json:"content"`
	IsError bool           `json:"is_error,omitempty"`
}

type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
}

type CommandResponseFromExt struct {
	Type      string     `json:"type"`
	ID        string     `json:"id"`
	Action    string     `json:"action"`
	Prompt    string     `json:"prompt,omitempty"`
	Insert    string     `json:"insert,omitempty"`
	Display   string     `json:"display,omitempty"`
	OpenPanel *PanelSpec `json:"open_panel,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type PanelSpec struct {
	ID     string   `json:"id"`
	Title  string   `json:"title,omitempty"`
	Lines  []string `json:"lines,omitempty"`
	Footer string   `json:"footer,omitempty"`
}

type PanelRenderFromExt struct {
	Type    string   `json:"type"`
	PanelID string   `json:"panel_id"`
	Title   string   `json:"title,omitempty"`
	Lines   []string `json:"lines,omitempty"`
	Footer  string   `json:"footer,omitempty"`
}

type PanelCloseFromExt struct {
	Type    string `json:"type"`
	PanelID string `json:"panel_id"`
}

type NotifyFromExt struct {
	Type    string `json:"type"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type ShutdownAckFromExt struct {
	Type string `json:"type"`
}

type HelloAckFromHost struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	ZotVersion      string `json:"zot_version"`
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	CWD             string `json:"cwd"`
	ExtensionDir    string `json:"extension_dir,omitempty"`
	DataDir         string `json:"data_dir,omitempty"`
}

type CommandInvokedFromHost struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

type ToolCallFromHost struct {
	Type string          `json:"type"`
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type EventFromHost struct {
	Type     string          `json:"type"`
	Event    string          `json:"event"`
	Step     int             `json:"step,omitempty"`
	Stop     string          `json:"stop,omitempty"`
	Error    string          `json:"error,omitempty"`
	ToolID   string          `json:"tool_id,omitempty"`
	ToolName string          `json:"tool_name,omitempty"`
	ToolArgs json.RawMessage `json:"tool_args,omitempty"`
	Text     string          `json:"text,omitempty"`
}

type EventInterceptFromHost struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	Event    string          `json:"event"`
	ToolID   string          `json:"tool_id,omitempty"`
	ToolName string          `json:"tool_name,omitempty"`
	ToolArgs json.RawMessage `json:"tool_args,omitempty"`
	Step     int             `json:"step,omitempty"`
	Text     string          `json:"text,omitempty"`
}

type PanelKeyFromHost struct {
	Type    string `json:"type"`
	PanelID string `json:"panel_id"`
	Key     string `json:"key"`
	Text    string `json:"text,omitempty"`
}

type PanelResizeFromHost struct {
	Type    string `json:"type"`
	PanelID string `json:"panel_id"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
}

type PanelCloseFromHost struct {
	Type    string `json:"type"`
	PanelID string `json:"panel_id"`
}

type ShutdownFromHost struct {
	Type string `json:"type"`
}

func Encode(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}
