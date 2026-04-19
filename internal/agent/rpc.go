package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/internal/agent/extensions"
	"github.com/patriceckhart/zot/internal/agent/modes"
	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/extproto"
	"github.com/patriceckhart/zot/internal/provider"
)

// runRPCMode implements the JSON-over-stdin/stdout RPC protocol.
//
// Wire format: one JSON object per line in both directions.
//
// Commands (stdin):
//
//	{"id":"1","type":"prompt","message":"hello","images":[]}
//	{"id":"2","type":"abort"}
//	{"id":"3","type":"compact"}
//	{"id":"4","type":"get_state"}
//	{"id":"5","type":"set_model","model":"claude-opus-4-5"}
//	{"id":"6","type":"get_messages"}
//	{"id":"7","type":"clear"}
//	{"id":"8","type":"get_models"}
//
// Responses (stdout): {"type":"response","id":"1","command":"prompt","success":true}
// Events (stdout): one JSON object per AgentEvent (same schema as --json mode).
//
// Auth: if $ZOTCORE_RPC_TOKEN is set, the first command must be
// {"type":"hello","token":"..."} or the connection is closed.
func runRPCMode(ctx context.Context, args Args, version string) error {
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}

	// Extensions: same lifecycle as interactive mode, minus the
	// host-hooks integration. Notify/Display calls from extensions
	// emit RPC events instead of TUI lines so any consumer can react.
	extHooks := &rpcExtHooks{}
	extMgr := extensions.New(ZotHome(), r.CWD, version, r.Provider, r.Model, extHooks)
	for _, e := range extMgr.LoadExplicit(ctx, args.Exts) {
		fmt.Fprintln(os.Stderr, "extension load:", e)
	}
	if !args.NoExt {
		for _, e := range extMgr.Discover(ctx) {
			fmt.Fprintln(os.Stderr, "extension load:", e)
		}
	}
	extMgr.WaitForReady(3 * time.Second)
	defer extMgr.Stop(2 * time.Second)
	r.MergeExtensionTools(&extToolAdapter{mgr: extMgr})

	ag := r.NewAgent()
	ag.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string) {
		return extMgr.InterceptToolCall(ctx, call.ID, call.Name, call.Arguments)
	}
	ag.OnEvent = func(ev core.AgentEvent) { fanoutAgentEvent(extMgr, ev) }
	extMgr.EmitEvent(extproto.EventFromHost{Event: "session_start"})

	server := &rpcServer{
		ctx:      ctx,
		args:     args,
		agent:    ag,
		provider: r.Provider,
		model:    r.Model,
		out:      os.Stdout,
		version:  version,
	}
	extHooks.server = server
	return server.run(os.Stdin)
}

// rpcExtHooks implements extensions.HostHooks for the headless RPC
// loop. Notify and Display surface as `event` frames so any RPC
// client can render them; Submit and Insert are no-ops because the
// RPC loop has no editor and the prompt comes from the client.
type rpcExtHooks struct {
	server *rpcServer
}

func (h *rpcExtHooks) Notify(extName, level, message string) {
	if h.server != nil {
		h.server.writeEvent(map[string]any{
			"type":      "ext_notify",
			"extension": extName,
			"level":     level,
			"message":   message,
		})
	}
}
func (h *rpcExtHooks) Display(extName, text string) {
	if h.server != nil {
		h.server.writeEvent(map[string]any{
			"type":      "ext_display",
			"extension": extName,
			"text":      text,
		})
	}
}
func (h *rpcExtHooks) Submit(string) {} // ignored in rpc mode
func (h *rpcExtHooks) Insert(string) {} // ignored in rpc mode

type rpcServer struct {
	ctx      context.Context
	args     Args
	agent    *core.Agent
	provider string
	model    string
	out      io.Writer
	version  string

	writeMu      sync.Mutex
	turnMu       sync.Mutex // serialises one prompt at a time
	activeCancel context.CancelFunc
	authed       bool

	// inFlight tracks long-running command goroutines so run() can
	// wait for them before returning when stdin closes. Without this,
	// piping a single 'prompt' command into 'zot rpc' would race the
	// process exit against the agent loop and the prompt would never
	// produce output.
	inFlight sync.WaitGroup
}

// run reads NDJSON commands from in and dispatches them. Returns when
// in is closed AND every in-flight long-running command (prompt /
// compact) has finished, so a quick `echo cmd | zot rpc` invocation
// still produces full output before the process exits.
func (s *rpcServer) run(in io.Reader) error {
	requireToken := os.Getenv("ZOTCORE_RPC_TOKEN") != ""
	s.authed = !requireToken

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var head struct {
			Type string `json:"type"`
			ID   string `json:"id,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &head); err != nil {
			s.writeError("", "", fmt.Sprintf("malformed json: %v", err))
			continue
		}
		if !s.authed {
			if head.Type != "hello" {
				s.writeError(head.ID, head.Type, "auth required: send hello with token first")
				continue
			}
			var hello struct {
				Token string `json:"token"`
			}
			_ = json.Unmarshal([]byte(line), &hello)
			if hello.Token != os.Getenv("ZOTCORE_RPC_TOKEN") {
				s.writeError(head.ID, head.Type, "invalid token")
				return fmt.Errorf("rpc: bad auth token")
			}
			s.authed = true
			s.writeResponse(head.ID, head.Type, map[string]any{
				"protocol_version": 1,
				"version":          s.version,
				"provider":         s.provider,
				"model":            s.model,
			})
			continue
		}
		s.dispatch(head.Type, head.ID, []byte(line))
	}
	err := sc.Err()
	s.inFlight.Wait()
	return err
}

// dispatch routes a command. Long-running commands (prompt, compact)
// run on their own goroutine so the read loop stays responsive.
func (s *rpcServer) dispatch(cmd, id string, raw []byte) {
	switch cmd {
	case "hello":
		s.writeResponse(id, cmd, map[string]any{
			"protocol_version": 1,
			"version":          s.version,
			"provider":         s.provider,
			"model":            s.model,
		})
	case "prompt":
		var req struct {
			Message string `json:"message"`
			Images  []struct {
				MimeType string `json:"mime_type"`
				Data     []byte `json:"data"`
			} `json:"images"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(id, cmd, err.Error())
			return
		}
		s.inFlight.Add(1)
		go func() {
			defer s.inFlight.Done()
			s.runPrompt(id, req.Message, req.Images)
		}()

	case "abort":
		if c := s.takeCancel(); c != nil {
			c()
		}
		s.writeResponse(id, cmd, nil)

	case "compact":
		s.inFlight.Add(1)
		go func() {
			defer s.inFlight.Done()
			s.runCompact(id)
		}()

	case "get_state":
		s.writeResponse(id, cmd, s.snapshotState())

	case "get_messages":
		s.writeResponse(id, cmd, map[string]any{
			"messages": messagesToJSON(s.agent.Messages()),
		})

	case "clear":
		s.agent.SetMessages(nil)
		s.writeResponse(id, cmd, nil)

	case "set_model":
		var req struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			s.writeError(id, cmd, err.Error())
			return
		}
		if _, err := provider.FindModel(s.provider, req.Model); err != nil {
			s.writeError(id, cmd, err.Error())
			return
		}
		s.agent.Model = req.Model
		s.model = req.Model
		s.writeResponse(id, cmd, map[string]any{"model": req.Model})

	case "get_models":
		out := []map[string]any{}
		for _, m := range provider.ModelsForProvider(s.provider) {
			out = append(out, map[string]any{
				"id":             m.ID,
				"provider":       m.Provider,
				"context_window": m.ContextWindow,
				"max_output":     m.MaxOutput,
				"reasoning":      m.Reasoning,
			})
		}
		s.writeResponse(id, cmd, map[string]any{"models": out})

	case "ping":
		s.writeResponse(id, cmd, map[string]any{"pong": true})

	default:
		s.writeError(id, cmd, "unknown command")
	}
}

// runPrompt executes a single prompt turn and streams events out.
// Holds turnMu so a second concurrent prompt blocks until this one
// finishes; the user can abort with the abort command.
func (s *rpcServer) runPrompt(id, message string, images []struct {
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	subCtx, cancel := context.WithCancel(s.ctx)
	s.setCancel(cancel)
	defer s.setCancel(nil)

	s.writeResponse(id, "prompt", map[string]any{"started": true})

	imgs := make([]provider.ImageBlock, 0, len(images))
	for _, im := range images {
		imgs = append(imgs, provider.ImageBlock{MimeType: im.MimeType, Data: im.Data})
	}

	err := s.agent.Prompt(subCtx, message, imgs, func(ev core.AgentEvent) {
		// EvDone is emitted by the agent loop and we re-emit our own
		// 'done' below; suppressing it here avoids duplicate frames.
		if _, ok := ev.(core.EvDone); ok {
			return
		}
		s.writeEvent(modes.EventToJSON(ev))
	})
	// Don't emit a stand-alone error event for cancellation; the prior
	// turn_end with stop=aborted already carries that signal.
	if err != nil && !errors.Is(err, context.Canceled) {
		s.writeEvent(map[string]any{"type": "error", "message": err.Error()})
	}
	s.writeEvent(map[string]any{"type": "done"})
}

// runCompact mirrors runPrompt but for compaction.
func (s *rpcServer) runCompact(id string) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()

	subCtx, cancel := context.WithCancel(s.ctx)
	s.setCancel(cancel)
	defer s.setCancel(nil)

	s.writeResponse(id, "compact", map[string]any{"started": true})
	summary, err := s.agent.Compact(subCtx, 4, nil)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.writeEvent(map[string]any{"type": "error", "message": err.Error()})
		}
		return
	}
	s.writeEvent(map[string]any{
		"type":    "compact_done",
		"summary": summary,
	})
}

// snapshotState builds the get_state response.
func (s *rpcServer) snapshotState() map[string]any {
	cum := s.agent.Cost()
	return map[string]any{
		"provider":      s.provider,
		"model":         s.model,
		"cwd":           s.args.CWD,
		"message_count": len(s.agent.Messages()),
		"busy":          s.activeCancel != nil,
		"usage": map[string]any{
			"input":       cum.InputTokens,
			"output":      cum.OutputTokens,
			"cache_read":  cum.CacheReadTokens,
			"cache_write": cum.CacheWriteTokens,
			"cost_usd":    cum.CostUSD,
		},
	}
}

// ---- write helpers (single-line JSON, mutex-guarded) ----

func (s *rpcServer) writeResponse(id, cmd string, data any) {
	frame := map[string]any{
		"type":    "response",
		"command": cmd,
		"success": true,
	}
	if id != "" {
		frame["id"] = id
	}
	if data != nil {
		frame["data"] = data
	}
	s.write(frame)
}

func (s *rpcServer) writeError(id, cmd, msg string) {
	frame := map[string]any{
		"type":    "response",
		"command": cmd,
		"success": false,
		"error":   msg,
	}
	if id != "" {
		frame["id"] = id
	}
	s.write(frame)
}

func (s *rpcServer) writeEvent(payload map[string]any) {
	s.write(payload)
}

func (s *rpcServer) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, _ = s.out.Write(b)
	_, _ = s.out.Write([]byte("\n"))
}

func (s *rpcServer) setCancel(c context.CancelFunc) {
	s.writeMu.Lock()
	s.activeCancel = c
	s.writeMu.Unlock()
}

func (s *rpcServer) takeCancel() context.CancelFunc {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	c := s.activeCancel
	s.activeCancel = nil
	return c
}

// messagesToJSON serialises a transcript using the same schema as the
// --json event mode for cross-format consistency.
func messagesToJSON(msgs []provider.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, map[string]any{
			"role":    string(m.Role),
			"content": modes.ContentToJSON(m.Content),
			"time":    m.Time,
		})
	}
	return out
}
