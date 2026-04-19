// Package extensions implements the host side of zot's subprocess
// extension protocol. The Manager discovers extensions in well-known
// directories, spawns each one, completes the hello handshake, and
// routes slash commands to the right extension.
//
// Each extension is its own process, communicating with zot over its
// stdin/stdout in newline-delimited JSON. Stderr is redirected to a
// per-extension log file under $ZOT_HOME/logs/. Crashing one
// extension does not affect the others or the host.
//
// See docs/extensions.md for the user-facing reference and
// internal/extproto for the wire format.
package extensions

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/patriceckhart/zot/internal/extproto"
)

// Manifest is the extension.json file shipped alongside an
// extension's executable. It tells zot how to launch the extension
// and provides display metadata.
type Manifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version,omitempty"`
	Exec        string   `json:"exec"`               // executable path, relative to manifest dir
	Args        []string `json:"args,omitempty"`     // extra argv passed to exec
	Language    string   `json:"language,omitempty"` // informational ("go", "python", "typescript", ...)
	Enabled     *bool    `json:"enabled,omitempty"`  // nil = enabled
	Description string   `json:"description,omitempty"`
}

// IsEnabled returns the manifest's effective enabled state. Default
// is true so adding a new extension folder Just Works without an
// extra zot ext enable command.
func (m Manifest) IsEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}

// Extension is a running extension subprocess and the metadata zot
// tracks about it.
type Extension struct {
	Manifest Manifest
	Dir      string // absolute path to extension directory
	LogPath  string

	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	logFile  *os.File
	helloAck bool
	commands []extproto.RegisterCommandFromExt
	tools    []extproto.RegisterToolFromExt

	// readyCh is closed when the extension sends a ReadyFromExt
	// frame, or when the host gives up waiting (registrationGrace).
	readyCh   chan struct{}
	readyOnce sync.Once

	// pending command invocations waiting on a CommandResponseFromExt
	// keyed by the id we sent in CommandInvokedFromHost.
	// pendingTool is the same idea for tool calls.
	// pendingIntercept is the same idea for event_intercept calls.
	mu               sync.Mutex
	pending          map[string]chan extproto.CommandResponseFromExt
	pendingTool      map[string]chan extproto.ToolResultFromExt
	pendingIntercept map[string]chan extproto.EventInterceptResponseFromExt

	// eventSubs and interceptSubs are the sets of event names this
	// extension subscribed to via SubscribeFromExt. Used by
	// EmitEvent / InterceptToolCall to filter recipients.
	eventSubs     map[string]struct{}
	interceptSubs map[string]struct{}
}

// HostHooks is the small interface the manager calls back into the
// running TUI through. Decouples extensions from interactive.go.
type HostHooks interface {
	// Notify pushes an ext-originated status message into the chat.
	// Level is one of "info", "warn", "error", "success".
	Notify(extName, level, message string)

	// Submit feeds text as if the user had typed and pressed enter,
	// running it through the agent loop.
	Submit(text string)

	// Insert places text at the cursor in the editor.
	Insert(text string)

	// Display appends a one-shot styled note to the chat without
	// invoking the model and without writing to the transcript.
	Display(extName, text string)
}

// Manager owns every extension subprocess for the lifetime of zot.
type Manager struct {
	zotHome    string
	cwd        string
	zotVersion string
	provider   string
	model      string
	hooks      HostHooks

	mu  sync.RWMutex
	ext map[string]*Extension // keyed by manifest name

	// commandIndex maps a slash-command name (without the leading /)
	// to the extension that registered it. First-come-first-served:
	// later registrations of the same command are dropped with a
	// warning.
	commandIndex map[string]*Extension

	// toolIndex maps an extension-defined tool name to its owning
	// extension. Same first-come-first-served rule as commandIndex.
	toolIndex map[string]*Extension
}

// New constructs an empty Manager. Call Discover to populate it from
// the on-disk extension directories.
func New(zotHome, cwd, zotVersion, provider, model string, hooks HostHooks) *Manager {
	return &Manager{
		zotHome:      zotHome,
		cwd:          cwd,
		zotVersion:   zotVersion,
		provider:     provider,
		model:        model,
		hooks:        hooks,
		ext:          map[string]*Extension{},
		commandIndex: map[string]*Extension{},
		toolIndex:    map[string]*Extension{},
	}
}

// Discover scans the global and project extension dirs and starts
// every extension whose manifest is enabled. Returns a slice of
// errors encountered (one per extension); a single bad extension
// doesn't abort the rest.
func (m *Manager) Discover(ctx context.Context) []error {
	var errs []error
	for _, dir := range m.searchDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // missing directory is fine
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			extDir := filepath.Join(dir, e.Name())
			if err := m.loadOne(ctx, extDir); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", extDir, err))
			}
		}
	}
	return errs
}

// searchDirs returns the directories the discoverer walks, in
// priority order: project-local first (so a project can override
// global behavior), then global.
func (m *Manager) searchDirs() []string {
	var dirs []string
	if m.cwd != "" {
		dirs = append(dirs, filepath.Join(m.cwd, ".zot", "extensions"))
	}
	if m.zotHome != "" {
		dirs = append(dirs, filepath.Join(m.zotHome, "extensions"))
	}
	return dirs
}

// loadOne reads a single extension's manifest and, if enabled,
// spawns its subprocess + completes the hello handshake.
func (m *Manager) loadOne(ctx context.Context, dir string) error {
	manifestPath := filepath.Join(dir, "extension.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var mf Manifest
	if err := json.Unmarshal(raw, &mf); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if mf.Name == "" {
		return errors.New("manifest: name is required")
	}
	if mf.Exec == "" {
		return errors.New("manifest: exec is required")
	}
	if !mf.IsEnabled() {
		// Quietly skip disabled extensions; zot ext list will show them.
		return nil
	}

	m.mu.RLock()
	_, dup := m.ext[mf.Name]
	m.mu.RUnlock()
	if dup {
		// Project-local copy already won; ignore the global one.
		return nil
	}

	ext := &Extension{
		Manifest:         mf,
		Dir:              dir,
		readyCh:          make(chan struct{}),
		pending:          map[string]chan extproto.CommandResponseFromExt{},
		pendingTool:      map[string]chan extproto.ToolResultFromExt{},
		pendingIntercept: map[string]chan extproto.EventInterceptResponseFromExt{},
		eventSubs:        map[string]struct{}{},
		interceptSubs:    map[string]struct{}{},
	}
	if err := m.spawn(ctx, ext); err != nil {
		return err
	}

	m.mu.Lock()
	m.ext[mf.Name] = ext
	// Note: ext.commands and ext.tools may be empty here — they're
	// populated by the read loop as register_* frames arrive after
	// hello. Indexing happens in the read loop too. Discover()'s
	// caller can WaitForReady() before relying on the registries.
	m.mu.Unlock()
	return nil
}

// WaitForReady blocks until every loaded extension has sent its
// ReadyFromExt sentinel, or the per-extension grace period expires.
// Call after Discover and before relying on tool registrations.
func (m *Manager) WaitForReady(grace time.Duration) {
	m.mu.RLock()
	exts := make([]*Extension, 0, len(m.ext))
	for _, e := range m.ext {
		exts = append(exts, e)
	}
	m.mu.RUnlock()
	for _, ext := range exts {
		select {
		case <-ext.readyCh:
		case <-time.After(grace):
			fmt.Fprintf(ext.logFile, "[zot] timed out waiting for ready frame; proceeding\n")
			ext.readyOnce.Do(func() { close(ext.readyCh) })
		}
	}
}

// spawn launches the subprocess, hooks up pipes, logs stderr, and
// runs the synchronous portion of the hello handshake. Asynchronous
// frames are processed in a goroutine started here.
func (m *Manager) spawn(ctx context.Context, ext *Extension) error {
	logsDir := filepath.Join(m.zotHome, "logs")
	_ = os.MkdirAll(logsDir, 0o755)
	logPath := filepath.Join(logsDir, "ext-"+ext.Manifest.Name+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	ext.LogPath = logPath
	ext.logFile = logFile
	fmt.Fprintf(logFile, "\n[zot] starting %s/%s at %s\n", ext.Manifest.Name, ext.Manifest.Version, time.Now().Format(time.RFC3339))

	// Exec resolution rules:
	//   - absolute path:                 used as-is.
	//   - starts with "." (./ or ../):  resolved relative to ext.Dir.
	//   - bare name (no path separator): looked up via $PATH so
	//                                    "node", "npx", "python3",
	//                                    "tsx" etc. work without
	//                                    forcing absolute paths.
	//   - other relative form (foo/bar): resolved relative to ext.Dir.
	execPath := ext.Manifest.Exec
	switch {
	case filepath.IsAbs(execPath):
		// keep
	case strings.HasPrefix(execPath, "."+string(filepath.Separator)) ||
		strings.HasPrefix(execPath, ".."+string(filepath.Separator)) ||
		execPath == "." || execPath == "..":
		execPath = filepath.Join(ext.Dir, execPath)
	case strings.ContainsRune(execPath, filepath.Separator):
		execPath = filepath.Join(ext.Dir, execPath)
	default:
		// bare name: leave as-is for exec.LookPath via exec.Command.
	}
	cmd := exec.CommandContext(ctx, execPath, ext.Manifest.Args...)
	cmd.Dir = ext.Dir
	cmd.Stderr = logFile

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	ext.cmd = cmd
	ext.stdin = stdin
	ext.stdout = stdout

	// Hello handshake. Read the extension's HelloFromExt synchronously
	// so we can fail fast on a broken extension; everything after is
	// processed in the read goroutine.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	if !scanner.Scan() {
		return fmt.Errorf("extension exited before hello: %w", scanner.Err())
	}
	var hello extproto.HelloFromExt
	if err := json.Unmarshal(scanner.Bytes(), &hello); err != nil {
		return fmt.Errorf("parse hello: %w", err)
	}
	if hello.Type != "hello" || hello.Name == "" {
		return fmt.Errorf("first frame must be hello (got %q)", hello.Type)
	}
	// Trust the manifest's name; ignore mismatch from the hello.
	ext.helloAck = true

	ack, _ := extproto.Encode(extproto.HelloAckFromHost{
		Type:            "hello_ack",
		ProtocolVersion: extproto.ProtocolVersion,
		ZotVersion:      m.zotVersion,
		Provider:        m.provider,
		Model:           m.model,
		CWD:             m.cwd,
	})
	if _, err := stdin.Write(ack); err != nil {
		return fmt.Errorf("send hello_ack: %w", err)
	}

	// Spin up the read loop now that the handshake is done.
	go m.readLoop(ext, scanner)
	return nil
}

// readLoop processes every frame the extension sends after hello.
// Returns when stdout closes.
func (m *Manager) readLoop(ext *Extension, scanner *bufio.Scanner) {
	defer func() {
		// On close, drop every command + tool this extension owned so
		// future invocations don't dangle. The subprocess is gone; we
		// won't hear back about its commands or tool calls anymore.
		m.mu.Lock()
		for name, owner := range m.commandIndex {
			if owner == ext {
				delete(m.commandIndex, name)
			}
		}
		for name, owner := range m.toolIndex {
			if owner == ext {
				delete(m.toolIndex, name)
			}
		}
		m.mu.Unlock()
		ext.readyOnce.Do(func() { close(ext.readyCh) })
		fmt.Fprintf(ext.logFile, "[zot] extension %s read loop exited at %s\n", ext.Manifest.Name, time.Now().Format(time.RFC3339))
	}()

	for scanner.Scan() {
		line := scanner.Bytes()
		var frame extproto.Frame
		if err := json.Unmarshal(line, &frame); err != nil {
			fmt.Fprintf(ext.logFile, "[zot] malformed json from extension: %v\n", err)
			continue
		}
		switch frame.Type {
		case "register_command":
			var rc extproto.RegisterCommandFromExt
			if err := json.Unmarshal(line, &rc); err == nil {
				ext.commands = append(ext.commands, rc)
				m.mu.Lock()
				if _, exists := m.commandIndex[rc.Name]; !exists {
					m.commandIndex[rc.Name] = ext
				}
				m.mu.Unlock()
			}
		case "register_tool":
			var rt extproto.RegisterToolFromExt
			if err := json.Unmarshal(line, &rt); err != nil {
				fmt.Fprintf(ext.logFile, "[zot] bad register_tool frame: %v\n", err)
				continue
			}
			// Validate the schema parses as JSON. If not, refuse to
			// register — a broken schema confuses the model.
			if len(rt.Schema) > 0 {
				var tmp any
				if err := json.Unmarshal(rt.Schema, &tmp); err != nil {
					fmt.Fprintf(ext.logFile, "[zot] tool %q: schema is not valid json (%v); skipped\n", rt.Name, err)
					continue
				}
			}
			ext.tools = append(ext.tools, rt)
			m.mu.Lock()
			if _, exists := m.toolIndex[rt.Name]; !exists {
				m.toolIndex[rt.Name] = ext
			}
			m.mu.Unlock()
		case "ready":
			ext.readyOnce.Do(func() { close(ext.readyCh) })
		case "subscribe":
			var sub extproto.SubscribeFromExt
			if err := json.Unmarshal(line, &sub); err == nil {
				ext.mu.Lock()
				for _, ev := range sub.Events {
					ext.eventSubs[ev] = struct{}{}
				}
				for _, ev := range sub.Intercept {
					if ev == "tool_call" { // only kind supported in v1
						ext.interceptSubs[ev] = struct{}{}
					}
				}
				ext.mu.Unlock()
			}
		case "event_intercept_response":
			var er extproto.EventInterceptResponseFromExt
			if err := json.Unmarshal(line, &er); err == nil {
				ext.mu.Lock()
				ch, ok := ext.pendingIntercept[er.ID]
				if ok {
					delete(ext.pendingIntercept, er.ID)
				}
				ext.mu.Unlock()
				if ok {
					select {
					case ch <- er:
					default:
					}
				}
			}
		case "tool_result":
			var tr extproto.ToolResultFromExt
			if err := json.Unmarshal(line, &tr); err == nil {
				ext.mu.Lock()
				ch, ok := ext.pendingTool[tr.ID]
				if ok {
					delete(ext.pendingTool, tr.ID)
				}
				ext.mu.Unlock()
				if ok {
					select {
					case ch <- tr:
					default:
					}
				}
			}
		case "notify":
			var n extproto.NotifyFromExt
			if err := json.Unmarshal(line, &n); err == nil {
				m.hooks.Notify(ext.Manifest.Name, n.Level, n.Message)
			}
		case "command_response":
			var cr extproto.CommandResponseFromExt
			if err := json.Unmarshal(line, &cr); err == nil {
				ext.mu.Lock()
				ch, ok := ext.pending[cr.ID]
				if ok {
					delete(ext.pending, cr.ID)
				}
				ext.mu.Unlock()
				if ok {
					select {
					case ch <- cr:
					default:
					}
				}
			}
		case "shutdown_ack":
			// Caller of Stop is waiting on the process exit, not this frame.
		default:
			fmt.Fprintf(ext.logFile, "[zot] unknown frame type %q\n", frame.Type)
		}
	}
}

// Commands returns a snapshot of every (extension, command) pair
// currently registered. Used by the slash autocomplete + /help.
func (m *Manager) Commands() []CommandInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []CommandInfo
	for _, ext := range m.ext {
		for _, c := range ext.commands {
			out = append(out, CommandInfo{
				Extension:   ext.Manifest.Name,
				Name:        c.Name,
				Description: c.Description,
			})
		}
	}
	return out
}

// CommandInfo is one extension-registered slash command, surfaced to
// the rest of zot for display purposes.
type CommandInfo struct {
	Extension   string
	Name        string
	Description string
}

// ToolInfo is one extension-registered tool. Used by the agent's
// build step to materialise core.Tool wrappers.
type ToolInfo struct {
	Extension   string
	Name        string
	Description string
	Schema      json.RawMessage
}

// Tools returns a snapshot of every (extension, tool) pair currently
// registered. Used at agent-build time to fold extension tools into
// the runtime tool registry.
func (m *Manager) Tools() []ToolInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ToolInfo
	for _, ext := range m.ext {
		for _, t := range ext.tools {
			out = append(out, ToolInfo{
				Extension:   ext.Manifest.Name,
				Name:        t.Name,
				Description: t.Description,
				Schema:      t.Schema,
			})
		}
	}
	return out
}

// HasTool reports whether name is registered by any extension.
func (m *Manager) HasTool(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.toolIndex[name]
	return ok
}

// InvokeTool sends a tool_call to the owning extension and waits for
// the matching tool_result. Used by the core.Tool wrapper that the
// agent registers per extension-defined tool.
func (m *Manager) InvokeTool(ctx context.Context, name string, args json.RawMessage, timeout time.Duration) (extproto.ToolResultFromExt, error) {
	m.mu.RLock()
	ext, ok := m.toolIndex[name]
	m.mu.RUnlock()
	if !ok {
		return extproto.ToolResultFromExt{}, fmt.Errorf("no extension registered for tool %q", name)
	}

	id := newCorrelationID()
	ch := make(chan extproto.ToolResultFromExt, 1)
	ext.mu.Lock()
	ext.pendingTool[id] = ch
	ext.mu.Unlock()

	frame, _ := extproto.Encode(extproto.ToolCallFromHost{
		Type: "tool_call",
		ID:   id,
		Name: name,
		Args: args,
	})
	if _, err := ext.stdin.Write(frame); err != nil {
		ext.mu.Lock()
		delete(ext.pendingTool, id)
		ext.mu.Unlock()
		return extproto.ToolResultFromExt{}, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		ext.mu.Lock()
		delete(ext.pendingTool, id)
		ext.mu.Unlock()
		return extproto.ToolResultFromExt{}, fmt.Errorf("timeout waiting for %s/%s", ext.Manifest.Name, name)
	case <-ctx.Done():
		ext.mu.Lock()
		delete(ext.pendingTool, id)
		ext.mu.Unlock()
		return extproto.ToolResultFromExt{}, ctx.Err()
	}
}

// HasCommand reports whether name is registered by any extension.
func (m *Manager) HasCommand(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, ok := m.commandIndex[name]
	return ok
}

// Invoke fires the named slash command's handler in the owning
// extension and waits up to timeout for the response. Returns the
// extension's CommandResponse so the caller can act on the action
// (prompt / insert / display).
func (m *Manager) Invoke(ctx context.Context, name, args string, timeout time.Duration) (extproto.CommandResponseFromExt, error) {
	m.mu.RLock()
	ext, ok := m.commandIndex[name]
	m.mu.RUnlock()
	if !ok {
		return extproto.CommandResponseFromExt{}, fmt.Errorf("no extension registered for /%s", name)
	}

	id := newCorrelationID()
	ch := make(chan extproto.CommandResponseFromExt, 1)
	ext.mu.Lock()
	ext.pending[id] = ch
	ext.mu.Unlock()

	frame, _ := extproto.Encode(extproto.CommandInvokedFromHost{
		Type: "command_invoked",
		ID:   id,
		Name: name,
		Args: args,
	})
	if _, err := ext.stdin.Write(frame); err != nil {
		ext.mu.Lock()
		delete(ext.pending, id)
		ext.mu.Unlock()
		return extproto.CommandResponseFromExt{}, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-time.After(timeout):
		ext.mu.Lock()
		delete(ext.pending, id)
		ext.mu.Unlock()
		return extproto.CommandResponseFromExt{}, fmt.Errorf("timeout waiting for %s/%s", ext.Manifest.Name, name)
	case <-ctx.Done():
		ext.mu.Lock()
		delete(ext.pending, id)
		ext.mu.Unlock()
		return extproto.CommandResponseFromExt{}, ctx.Err()
	}
}

// Stop cleanly terminates every extension. Sends ShutdownFromHost,
// waits up to gracePeriod for each subprocess to exit, then SIGTERMs
// (and SIGKILLs after another second) the holdouts.
func (m *Manager) Stop(gracePeriod time.Duration) {
	m.mu.RLock()
	exts := make([]*Extension, 0, len(m.ext))
	for _, e := range m.ext {
		exts = append(exts, e)
	}
	m.mu.RUnlock()

	for _, ext := range exts {
		if frame, err := extproto.Encode(extproto.ShutdownFromHost{Type: "shutdown"}); err == nil {
			_, _ = ext.stdin.Write(frame)
		}
		_ = ext.stdin.Close()
	}

	deadline := time.Now().Add(gracePeriod)
	for _, ext := range exts {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = 100 * time.Millisecond
		}
		done := make(chan struct{})
		go func() { _ = ext.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(remaining):
			_ = ext.cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(time.Second):
				_ = ext.cmd.Process.Kill()
				<-done
			}
		}
		if ext.logFile != nil {
			_ = ext.logFile.Close()
		}
	}
}

// All returns every extension currently tracked, enabled or not.
// Used by `zot ext list`.
func (m *Manager) All() []*Extension {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Extension, 0, len(m.ext))
	for _, e := range m.ext {
		out = append(out, e)
	}
	return out
}

// newCorrelationID returns a short non-cryptographic id. We don't
// need uniqueness across processes, just within the lifetime of one
// extension's pending map.
func newCorrelationID() string {
	return strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
}
