package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
	"github.com/patriceckhart/zot/packages/agent/modes"
	"github.com/patriceckhart/zot/packages/agent/skills"
	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/provider/auth"
	"github.com/patriceckhart/zot/packages/tui"
)

// interactiveExtHooks is a tiny adapter that lets the extension
// manager call back into the Interactive instance built later in
// runInteractive. The forward-declared *modes.Interactive is filled
// in immediately after manager construction.
type interactiveExtHooks struct {
	ivPtr **modes.Interactive
}

func (h *interactiveExtHooks) iv() *modes.Interactive {
	if h == nil || h.ivPtr == nil {
		return nil
	}
	return *h.ivPtr
}

func (h *interactiveExtHooks) Notify(extName, level, message string) {
	if iv := h.iv(); iv != nil {
		iv.Notify(extName, level, message)
	}
}
func (h *interactiveExtHooks) Submit(text string) {
	if iv := h.iv(); iv != nil {
		iv.SubmitOrQueue(text, nil)
	}
}
func (h *interactiveExtHooks) SubmitSlash(text string) {
	if iv := h.iv(); iv != nil {
		iv.SubmitSlash(text)
	}
}
func (h *interactiveExtHooks) Insert(text string) {
	if iv := h.iv(); iv != nil {
		iv.Insert(text)
	}
}
func (h *interactiveExtHooks) Display(extName, text string) {
	if iv := h.iv(); iv != nil {
		iv.Display(extName, text)
	}
}
func (h *interactiveExtHooks) ClearNotes(extName string) {
	if iv := h.iv(); iv != nil {
		iv.ClearNotes(extName)
	}
}
func (h *interactiveExtHooks) OpenPanel(extName string, spec extproto.PanelSpec) {
	if iv := h.iv(); iv != nil {
		iv.OpenPanel(extName, spec)
	}
}
func (h *interactiveExtHooks) UpdatePanel(extName, panelID, title string, lines []string, footer string) {
	if iv := h.iv(); iv != nil {
		iv.UpdatePanel(extName, panelID, title, lines, footer)
	}
}
func (h *interactiveExtHooks) ClosePanel(extName, panelID string) {
	if iv := h.iv(); iv != nil {
		iv.ClosePanel(extName, panelID)
	}
}

// extToolAdapter bridges *extensions.Manager to the
// ExtensionToolSource interface declared in build.go (kept narrow to
// avoid a build->extensions import cycle). One adapter instance per
// run; used at every Resolve point so re-built agents pick up the
// same set of extension tools.
type extToolAdapter struct {
	mgr *extensions.Manager
}

func (a *extToolAdapter) Tools() []ExtensionToolInfo {
	infos := a.mgr.Tools()
	out := make([]ExtensionToolInfo, len(infos))
	for i, t := range infos {
		out[i] = ExtensionToolInfo{
			Extension:   t.Extension,
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Schema,
			Deferred:    t.Deferred,
		}
	}
	return out
}

func (a *extToolAdapter) NewExtensionTool(info ExtensionToolInfo) core.Tool {
	return extensions.NewTool(a.mgr, extensions.ToolInfo{
		Extension:   info.Extension,
		Name:        info.Name,
		Description: info.Description,
		Schema:      info.Schema,
		Deferred:    info.Deferred,
	})
}

// liveInteractiveAgent returns the agent currently owned by the TUI. The
// fallback is only used before the Interactive has been constructed.
//
// Agent rebuilds, such as cross-provider model switches, replace the TUI's
// agent without replacing the startup pointer held by runInteractive. Session
// operations must therefore resolve the live agent at the time of the action.
func liveInteractiveAgent(iv *modes.Interactive, fallback *core.Agent) *core.Agent {
	if iv != nil {
		return iv.Agent()
	}
	return fallback
}

func trimMessagesForResume(msgs []provider.Message, keepTail int) []provider.Message {
	if keepTail <= 0 || len(msgs) <= keepTail {
		return provider.RepairOrphanedToolResults(msgs)
	}
	var out []provider.Message
	start := len(msgs) - keepTail
	// Preserve the synthetic compaction summary when present so an
	// already-compacted session stays compacted after resume.
	if len(msgs) > 0 && msgs[0].Meta["compaction"] == "true" && start > 1 {
		out = append(out, msgs[0])
	}
	// Avoid hydrating a tail that starts with orphan tool_result rows;
	// provider APIs require those to be paired with an earlier tool_use.
	for start < len(msgs) && msgs[start].Role == provider.RoleTool {
		start++
	}
	out = append(out, msgs[start:]...)
	return provider.RepairOrphanedToolResults(out)
}

// fanoutAgentEvent translates a core.AgentEvent into the wire-format
// EventFromHost and pushes it through the extension manager. Only
// the events that have a clear extension-facing meaning are
// forwarded; internal-only ones (text_delta, tool_progress) are
// dropped to keep the per-extension stream sane.
func fanoutAgentEvent(mgr *extensions.Manager, ev core.AgentEvent) {
	if mgr == nil {
		return
	}
	switch e := ev.(type) {
	case core.EvTurnStart:
		mgr.EmitEvent(extproto.EventFromHost{Event: "turn_start", Step: e.Step})
	case core.EvToolCall:
		mgr.EmitEvent(extproto.EventFromHost{
			Event: "tool_call", ToolID: e.ID, ToolName: e.Name, ToolArgs: e.Args,
		})
	case core.EvAssistantMessage:
		// Concat the visible text portions of the message; binary
		// blocks (tool_use, etc.) are skipped because subscribers
		// usually want a string they can grep / display.
		var text string
		for _, c := range e.Message.Content {
			if tb, ok := c.(provider.TextBlock); ok {
				text += tb.Text
			}
		}
		mgr.EmitEvent(extproto.EventFromHost{Event: "assistant_message", Text: text})
	case core.EvTurnEnd:
		ev := extproto.EventFromHost{Event: "turn_end", Stop: string(e.Stop)}
		if e.Err != nil {
			ev.Error = e.Err.Error()
		}
		mgr.EmitEvent(ev)
	}
}

// Run is the top-level entrypoint for the zot binary.
func Run(rawArgs []string, version string) error {
	// Apply network configuration before any subcommand can make an HTTP
	// request. Standard proxy environment variables retain precedence.
	applyConfiguredHTTPProxy()

	// Subcommand router: `zot bot ...` is handled separately so the
	// generic flag parser doesn't reject "bot" as a positional arg.
	if handled, err := runBotCommand(rawArgs, version); handled {
		return err
	}
	if handled, err := runExtCommand(rawArgs, version); handled {
		return err
	}
	if handled, err := runUpdateCommand(rawArgs, version); handled {
		return err
	}
	if handled, err := runSessionsCommand(rawArgs); handled {
		return err
	}
	if handled, err := runZotfileCommand(rawArgs, version); handled {
		return err
	}
	return runWithArgsRaw(rawArgs, version)
}

func runWithArgsRaw(rawArgs []string, version string) error {
	// `zot rpc` is shorthand for `zot --rpc` so third-party apps can
	// spawn the binary with a clean argv. Strip the leading 'rpc'
	// token and let the rest flow through the normal arg parser.
	if len(rawArgs) > 0 && rawArgs[0] == "rpc" {
		rawArgs = append([]string{"--rpc"}, rawArgs[1:]...)
	}

	if fi, err := os.Stdin.Stat(); err == nil {
		rawArgs = argsForStdin(rawArgs, fi.Mode())
	}
	args, err := ParseArgs(rawArgs)
	if err != nil {
		PrintHelp(version)
		return err
	}
	if args.Help {
		printHelp(os.Stdout, version)
		return nil
	}
	if args.Version {
		fmt.Println("zot", version)
		return nil
	}
	// Model catalog: load any cached discovery data before we inspect
	// the model list (list-models, print/json, interactive).
	prepareRuntimeCatalog()

	if args.ListModels {
		printModels()
		return nil
	}

	return runWithArgs(args, version)
}

func runWithArgs(args Args, version string) error {
	ctx := context.Background()
	switch args.Mode {
	case ModePrint:
		return runPrintMode(ctx, args, version)
	case ModeStream:
		return runStreamMode(ctx, args, version)
	case ModeJSON:
		return runJSONMode(ctx, args, version)
	case ModeRPC:
		return runRPCMode(ctx, args, version)
	case ModeSwarmAgent:
		return runSwarmAgentMode(ctx, args, version)
	default:
		return runInteractive(ctx, args, version)
	}
}

// ---- print / json modes: require credentials, run single-shot ----

// nonInteractiveExtHooks is the HostHooks impl used by print / json
// modes. They have no TUI, so notify / display go to stderr and
// submit / insert are no-ops (the extension can't steer a
// single-shot run once it's in flight anyway).
type nonInteractiveExtHooks struct{}

func (nonInteractiveExtHooks) Notify(ext, level, message string) {
	fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", ext, level, message)
}
func (nonInteractiveExtHooks) Submit(string)                                        {}
func (nonInteractiveExtHooks) SubmitSlash(string)                                   {}
func (nonInteractiveExtHooks) Insert(string)                                        {}
func (nonInteractiveExtHooks) Display(string, string)                               {}
func (nonInteractiveExtHooks) ClearNotes(string)                                    {}
func (nonInteractiveExtHooks) OpenPanel(string, extproto.PanelSpec)                 {}
func (nonInteractiveExtHooks) UpdatePanel(string, string, string, []string, string) {}
func (nonInteractiveExtHooks) ClosePanel(string, string)                            {}

// setupNonInteractiveExtensions loads --ext paths and (unless
// --no-ext) runs discovery. Returns the manager so the caller can
// wire tools into the resolved registry, and a cleanup closure to
// defer. Mirrors the interactive-mode setup minus the TUI hooks.
func setupNonInteractiveExtensions(ctx context.Context, args Args, r *Resolved, version string) (*extensions.Manager, func()) {
	reportSkillDiagnostics(os.Stderr, r.SkillDiagnostics)
	extMgr := extensions.New(ZotHome(), r.CWD, version, r.Provider, r.Model, nonInteractiveExtHooks{})
	for _, e := range extMgr.LoadExplicit(ctx, args.Exts) {
		fmt.Fprintln(os.Stderr, "extension load:", e)
	}
	if !args.NoExt {
		for _, e := range extMgr.Discover(ctx) {
			fmt.Fprintln(os.Stderr, "extension load:", e)
		}
	}
	extMgr.WaitForReady(3 * time.Second)
	r.MergeExtensionTools(&extToolAdapter{mgr: extMgr})
	extMgr.EmitEvent(extproto.EventFromHost{Event: "session_start"})
	return extMgr, func() { extMgr.Stop(2 * time.Second) }
}

func reportSkillDiagnostics(w io.Writer, diagnostics []string) {
	for _, diagnostic := range diagnostics {
		fmt.Fprintln(w, "skill discovery:", diagnostic)
	}
}

// wireNonInteractiveAgentExtHooks installs the same BeforeToolExecute
// / BeforeTurn / BeforeAssistantMessage / OnEvent hooks the
// interactive path wires up, so extensions get their normal
// event-intercept surface in print / json / rpc flows too.
func wireNonInteractiveAgentExtHooks(ctx context.Context, ag *core.Agent, extMgr *extensions.Manager) {
	if ag == nil || extMgr == nil {
		return
	}
	wireBeforeAgentStart(ag, extMgr, "")
	ag.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
		res := extMgr.InterceptToolCall(ctx, call.ID, call.Name, call.Arguments)
		if res.Block {
			return false, res.Reason, nil
		}
		return true, "", res.ModifiedArgs
	}
	ag.BeforeTurn = func(step int) (bool, string) {
		res := extMgr.InterceptTurnStart(ctx, step)
		return !res.Block, res.Reason
	}
	ag.BeforeAssistantMessage = func(text string) (bool, string, string) {
		res := extMgr.InterceptAssistantMessage(ctx, text)
		if res.Block {
			return false, res.Reason, ""
		}
		return true, "", res.ReplaceText
	}
	ag.OnEvent = func(ev core.AgentEvent) { fanoutAgentEvent(extMgr, ev) }
}

type printStats struct {
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	PromptTokens          int    `json:"prompt_tokens"`
	ReasoningTokens       *int   `json:"reasoning_tokens"`
	GeneratedOutputTokens int    `json:"generated_output_tokens"`
	ElapsedMS             int64  `json:"elapsed_ms"`
}

func writePrintStats(path, providerID, model string, usage provider.Usage, elapsed time.Duration) error {
	var reasoning *int
	generated := usage.OutputTokens
	if usage.ReasoningTokensKnown {
		count := usage.ReasoningTokens
		reasoning = &count
		generated -= count
		if generated < 0 {
			generated = 0
		}
	}
	stats := printStats{
		Provider:              providerID,
		Model:                 model,
		PromptTokens:          usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens,
		ReasoningTokens:       reasoning,
		GeneratedOutputTokens: generated,
		ElapsedMS:             elapsed.Milliseconds(),
	}
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return fmt.Errorf("encode stats: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write stats %q: %w", path, err)
	}
	return nil
}

func runPrintMode(ctx context.Context, args Args, version string) error {
	if args.NoYolo {
		fmt.Fprintln(os.Stderr, "warning: --no-yolo has no effect in print mode (no interactive prompt available); tools will run without confirmation")
	}
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, args, &r, version)
	defer stopExt()

	ag := r.NewAgent()
	wireNonInteractiveAgentExtHooks(ctx, ag, extMgr)
	sess, _ := openOrCreateSession(args, r, ag, version)
	defer sess.Close()

	piped, _ := readAllStdin()
	prompt := combinePromptInput(piped, args.Prompt)
	if prompt == "" {
		return fmt.Errorf("print mode requires a prompt (arg or stdin)")
	}

	start := len(ag.Messages())
	if err := runZotfileStartupPre(ctx, args.StartupPre, r.CWD, r.Sandbox, ag, nil, os.Stderr); err != nil {
		return err
	}
	reloadResourcesAfterStartupPre(ctx, args, extMgr, r.Sandbox, ag)
	started := time.Now()
	usage, err := modes.RunPrint(ctx, ag, prompt, nil, os.Stdout)
	elapsed := time.Since(started)
	WriteNewTranscript(ag, sess, start)
	if err != nil {
		return err
	}
	if args.StatsPath != "" {
		return writePrintStats(args.StatsPath, r.Provider, r.Model, usage, elapsed)
	}
	return nil
}

func runStreamMode(ctx context.Context, args Args, version string) error {
	if args.NoYolo {
		fmt.Fprintln(os.Stderr, "warning: --no-yolo has no effect in stream mode (no interactive prompt available); tools will run without confirmation")
	}
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, args, &r, version)
	defer stopExt()

	ag := r.NewAgent()
	wireNonInteractiveAgentExtHooks(ctx, ag, extMgr)
	sess, _ := openOrCreateSession(args, r, ag, version)
	defer sess.Close()

	piped, _ := readAllStdin()
	prompt := combinePromptInput(piped, args.Prompt)
	if prompt == "" {
		return fmt.Errorf("stream mode requires a prompt (arg or stdin)")
	}

	start := len(ag.Messages())
	preSink, finishPre := newStreamTextSink(os.Stdout)
	if err := runZotfileStartupPre(ctx, args.StartupPre, r.CWD, r.Sandbox, ag, preSink, os.Stderr); err != nil {
		finishPre()
		return err
	}
	finishPre()
	reloadResourcesAfterStartupPre(ctx, args, extMgr, r.Sandbox, ag)
	err = modes.RunStream(ctx, ag, prompt, nil, os.Stdout)
	WriteNewTranscript(ag, sess, start)
	return err
}

func newStreamTextSink(out io.Writer) (func(core.AgentEvent), func()) {
	var streamed, wroteText, lastWasNL bool
	writeText := func(text string) {
		if text == "" {
			return
		}
		_, _ = fmt.Fprint(out, text)
		if syncer, ok := out.(interface{ Sync() error }); ok {
			_ = syncer.Sync()
		}
		wroteText = true
		lastWasNL = strings.HasSuffix(text, "\n")
	}
	sink := func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvAssistantStart:
			streamed = false
		case core.EvTextDelta:
			streamed = true
			writeText(e.Delta)
		case core.EvAssistantMessage:
			if streamed {
				return
			}
			var text strings.Builder
			for _, content := range e.Message.Content {
				if block, ok := content.(provider.TextBlock); ok {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(block.Text)
				}
			}
			writeText(text.String())
		}
	}
	finish := func() {
		if wroteText && !lastWasNL {
			writeText("\n")
		}
	}
	return sink, finish
}

func runJSONMode(ctx context.Context, args Args, version string) error {
	if args.NoYolo {
		fmt.Fprintln(os.Stderr, "warning: --no-yolo has no effect in json mode (no interactive prompt available); tools will run without confirmation")
	}
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, args, &r, version)
	defer stopExt()

	ag := r.NewAgent()
	wireNonInteractiveAgentExtHooks(ctx, ag, extMgr)
	sess, _ := openOrCreateSession(args, r, ag, version)
	defer sess.Close()

	piped, _ := readAllStdin()
	prompt := combinePromptInput(piped, args.Prompt)
	if prompt == "" {
		return fmt.Errorf("json mode requires a prompt (arg or stdin)")
	}

	start := len(ag.Messages())
	enc := json.NewEncoder(os.Stdout)
	preSink := func(ev core.AgentEvent) {
		_ = enc.Encode(modes.EventToJSON(ev))
	}
	if err := runZotfileStartupPre(ctx, args.StartupPre, r.CWD, r.Sandbox, ag, preSink, os.Stderr); err != nil {
		_ = enc.Encode(map[string]any{"type": "error", "message": err.Error()})
		return err
	}
	reloadResourcesAfterStartupPre(ctx, args, extMgr, r.Sandbox, ag)
	err = modes.RunJSON(ctx, ag, prompt, nil, os.Stdout)
	WriteNewTranscript(ag, sess, start)
	return err
}

// runZotfileStartupPre runs entry.pre before the main non-interactive prompt.
// "!command" values execute via BashTool; other values are sent as an agent turn.
// shellOut receives live shell chunks (typically os.Stderr); sink receives
// agent events for plain-text pre (stream mode wires a live text sink).
func runZotfileStartupPre(ctx context.Context, pre, cwd string, sandbox *tools.Sandbox, ag *core.Agent, sink func(core.AgentEvent), shellOut io.Writer) error {
	pre = strings.TrimSpace(pre)
	if pre == "" {
		return nil
	}
	if cmd, ok := modes.ShellEscapeCommand(pre); ok {
		raw, err := json.Marshal(map[string]any{"command": cmd})
		if err != nil {
			return err
		}
		if shellOut != nil {
			fmt.Fprintf(shellOut, "$ %s\n", cmd)
			if f, ok := shellOut.(interface{ Sync() error }); ok {
				_ = f.Sync()
			}
		}
		progress := func(chunk string) {
			if shellOut == nil || chunk == "" {
				return
			}
			fmt.Fprint(shellOut, chunk)
			if f, ok := shellOut.(interface{ Sync() error }); ok {
				_ = f.Sync()
			}
		}
		bash := &tools.BashTool{CWD: cwd, Sandbox: sandbox}
		res, err := bash.Execute(ctx, raw, progress)
		if err != nil {
			return fmt.Errorf("zotfile entry.pre: %w", err)
		}
		if res.IsError {
			var sb strings.Builder
			for _, c := range res.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					sb.WriteString(tb.Text)
				}
			}
			msg := strings.TrimSpace(sb.String())
			if msg == "" {
				msg = "command failed"
			}
			return fmt.Errorf("zotfile entry.pre: %s", msg)
		}
		return nil
	}
	if ag == nil {
		return fmt.Errorf("zotfile entry.pre requires an agent for non-shell prompts")
	}
	if sink == nil {
		sink = func(core.AgentEvent) {}
	}
	return ag.Prompt(ctx, pre, nil, sink)
}

// refreshAgentToolsAndPrompt re-resolves tools (including rediscovered
// skills and currently loaded extension tools) and updates the live
// agent's registry and system prompt. Used after /reload-ext and after
// zotfile entry.pre installs new skills or extensions.
// mutateRegistry, if non-nil, can inject session-specific tools (e.g. swarm_spawn).
func refreshAgentToolsAndPrompt(args Args, sharedSandbox *tools.Sandbox, extToolAdapter ExtensionToolSource, ag *core.Agent, mutateRegistry func(core.Registry) core.Registry) {
	if ag == nil {
		return
	}
	resolved, err := Resolve(args, true)
	if err != nil {
		return
	}
	if sharedSandbox != nil {
		resolved.UseSandbox(sharedSandbox)
	}
	if extToolAdapter != nil {
		resolved.MergeExtensionTools(extToolAdapter)
	}
	reg := resolved.ToolRegistry
	if mutateRegistry != nil {
		reg = mutateRegistry(reg)
	}
	ag.SetTools(reg)
	ag.SetSystem(resolved.SystemPrompt)
}

// reloadResourcesAfterStartupPre reloads extensions (if any) and
// refreshes the agent tool registry + system prompt so skills/extensions
// installed by entry.pre are visible to the following turn.
func reloadResourcesAfterStartupPre(ctx context.Context, args Args, extMgr *extensions.Manager, sharedSandbox *tools.Sandbox, ag *core.Agent) {
	if strings.TrimSpace(args.StartupPre) == "" || ag == nil {
		return
	}
	adapter := &extToolAdapter{mgr: extMgr}
	if extMgr != nil {
		_ = extMgr.Reload(ctx, 2*time.Second)
	}
	refreshAgentToolsAndPrompt(args, sharedSandbox, adapter, ag, nil)
}

// ---- interactive mode: opens the TUI even without credentials ----

func runInteractive(ctx context.Context, args Args, version string) error {
	// Resolve WITHOUT requiring credentials.
	r, err := Resolve(args, false)
	if err != nil {
		return err
	}

	authStore := AuthStoreFor()
	mgr := auth.NewManager(authStore)
	defer mgr.Close()

	// Keep the sandbox pointer stable across agent rebuilds (login / model
	// switch). The Interactive UI toggles the lock via this pointer, and
	// rebuilt tool instances must share the same one so the lock sticks.
	sharedSandbox := r.Sandbox

	// Build the extension manager BEFORE the agent so we can fold
	// extension-defined tools into the registry. Forward-declare iv so
	// the host hooks adapter can dereference it after construction.
	var iv *modes.Interactive
	extHooks := &interactiveExtHooks{ivPtr: &iv}
	extMgr := extensions.New(ZotHome(), r.CWD, version, r.Provider, r.Model, extHooks)
	startupExtensionErrors := make([]string, 0, len(r.SkillDiagnostics))
	for _, diagnostic := range r.SkillDiagnostics {
		startupExtensionErrors = append(startupExtensionErrors, "skill discovery: "+diagnostic)
	}
	// --ext paths first so they win against installed extensions of
	// the same name (loadOne's first-write-wins semantics).
	for _, e := range extMgr.LoadExplicit(ctx, args.Exts) {
		startupExtensionErrors = append(startupExtensionErrors, e.Error())
	}
	// --no-ext skips the global + project-local discovery scan;
	// explicit --ext paths above are still honoured so you can run
	// "only this extension" with --no-ext --ext ./x.
	if !args.NoExt {
		for _, e := range extMgr.Discover(ctx) {
			startupExtensionErrors = append(startupExtensionErrors, e.Error())
		}
	}
	// Wait briefly for extensions to flush their initial register_tool
	// frames before we build the agent's tool registry. Half a second
	// is plenty for any extension that's actually well-behaved; ones
	// that don't send a ready frame eat the full grace and proceed.
	// 3s is the per-extension grace period for the ready frame.
	// Native binaries are instant; runtimes like `npx tsx` take ~1.5s
	// from cold cache. The wait is tight only for extensions that
	// haven't sent ready by then; ones that signalled earlier release
	// the wait immediately.
	extMgr.WaitForReady(3 * time.Second)
	defer extMgr.Stop(2 * time.Second)

	extToolAdapter := &extToolAdapter{mgr: extMgr}
	r.MergeExtensionTools(extToolAdapter)

	// Build the swarm supervisor BEFORE the agent so the auto-swarm
	// tool can reference it during tool-registry construction. State
	// lives under ZotHome/swarm so per-agent meta/events survive
	// restarts; the user can hunt orphaned agents down with
	// `git worktree list` if anything misbehaves.
	//
	// swarmMgr is also captured by loadSession / changeCWD closures
	// further down the function, which is why we keep the variable
	// in this outer scope rather than scoping it tighter.
	var swarmMgr *swarm.Swarm
	swarmMgr = swarm.New(swarm.Config{
		Root:     filepath.Join(ZotHome(), "swarm"),
		RepoRoot: r.CWD,
		ResolveCredential: func(ctx context.Context, providerID string) (swarm.Credential, error) {
			if providerID == "ollama" {
				return swarm.Credential{Value: "ollama", Method: "apikey"}, nil
			}
			credential, method, accountID, err := ResolveCredentialFullContext(ctx, providerID, "")
			if err != nil {
				// Providers backed only by a local endpoint do not need a key.
				// Leave stdin untouched and let the child resolve that endpoint.
				if !CredentialAvailable(providerID) {
					return swarm.Credential{}, nil
				}
				return swarm.Credential{}, err
			}
			return swarm.Credential{Value: credential, Method: method, AccountID: accountID}, nil
		},
	})
	// Pull any previously-spawned agents off disk so the dashboard
	// shows them as detached and the user can resume / remove them.
	_, _ = swarmMgr.Reload()

	// onSpawnedSwarm is the OnSpawned callback the swarm_spawn tool
	// fires after every successful spawn. It hands the agent off to
	// the running Interactive so the watcher can flush a summary back
	// into chat when all sub-agents finish. Reads `iv` lazily because
	// the Interactive is constructed after the agent.
	onSpawnedSwarm := func(a *swarm.Agent, task string) {
		if iv != nil {
			iv.TrackSwarmAgent(a, task)
		}
	}

	// Inject the swarm_spawn auto-swarm tool only when /settings ->
	// auto-swarm is currently enabled. Registering it unconditionally
	// leaves the model trying to call it (and getting a polite error)
	// even when the user has switched the feature off. The /settings
	// toggle live-mutates the running agent's registry separately so
	// flipping the flag mid-session takes effect on the next turn.
	injectSwarmSpawn := func(reg core.Registry) core.Registry {
		if reg == nil {
			return reg
		}
		if !AutoSwarmEnabled() {
			return reg
		}
		reg["swarm_spawn"] = &tools.SwarmSpawnTool{
			Swarm:           swarmMgr,
			Enabled:         AutoSwarmEnabled,
			DefaultModel:    func() string { return r.Model },
			DefaultProvider: func() string { return r.Provider },
			OnSpawned:       onSpawnedSwarm,
		}
		return reg
	}
	injectSwarmSpawn(r.ToolRegistry)

	// Confirmation gate: when --no-yolo is on, the agent must ask
	// the user before every tool call. In interactive mode the TUI
	// provides the Confirmer; in print/json/rpc modes there's no
	// way to prompt, so the gate is constructed with a nil inner
	// which auto-refuses every call with a helpful reason.
	var confirmGate *core.ConfirmGate
	if args.NoYolo {
		confirmGate = core.NewConfirmGate(nil) // set below for interactive
	}

	// Capture current args in a closure so BuildAgent can re-resolve
	// after a successful login (picks up the newly stored credential).
	wireAgentExt := func(resolved Resolved) *core.Agent {
		a := resolved.NewAgent()
		wireBeforeAgentStart(a, extMgr, resolved.Provider)
		a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
			// Guards run before confirmation because they may rewrite the
			// arguments. The user must approve the effective call that will
			// actually execute, not the model's original arguments.
			r := extMgr.InterceptToolCall(ctx, call.ID, call.Name, call.Arguments)
			if r.Block {
				return false, r.Reason, nil
			}
			effectiveArgs := call.Arguments
			if len(r.ModifiedArgs) > 0 && json.Valid(r.ModifiedArgs) {
				effectiveArgs = r.ModifiedArgs
			}
			if confirmGate != nil {
				var content strings.Builder
				if tool, err := a.Tools.Get(call.Name); err == nil {
					if previewer, ok := tool.(core.ToolPreviewer); ok {
						preview, err := previewer.Preview(ctx, effectiveArgs)
						if err != nil {
							return false, err.Error(), nil
						}
						for _, block := range preview.Content {
							if text, ok := block.(provider.TextBlock); ok {
								if content.Len() > 0 {
									content.WriteString("\n")
								}
								content.WriteString(text.Text)
							}
						}
					}
				}
				ok, reason, _ := confirmGate.CheckToolCall(core.ToolCallConfirmation{
					ID:      call.ID,
					Name:    call.Name,
					Summary: core.BuildPreview(effectiveArgs, 120),
					Content: content.String(),
				})
				if !ok {
					return false, reason, nil
				}
			}
			return true, "", r.ModifiedArgs
		}
		a.BeforeTurn = func(step int) (bool, string) {
			r := extMgr.InterceptTurnStart(ctx, step)
			return !r.Block, r.Reason
		}
		a.BeforeAssistantMessage = func(text string) (bool, string, string) {
			r := extMgr.InterceptAssistantMessage(ctx, text)
			if r.Block {
				return false, r.Reason, ""
			}
			return true, "", r.ReplaceText
		}
		a.OnEvent = func(ev core.AgentEvent) { fanoutAgentEvent(extMgr, ev) }
		return a
	}

	buildAgent := func() (*core.Agent, string, string, error) {
		resolved, err := Resolve(args, true)
		if err != nil {
			return nil, "", "", err
		}
		resolved.UseSandbox(sharedSandbox)
		resolved.MergeExtensionTools(extToolAdapter)
		injectSwarmSpawn(resolved.ToolRegistry)
		return wireAgentExt(resolved), resolved.Provider, resolved.Model, nil
	}

	// Rebuild agent with an explicit provider/model override.
	buildAgentFor := func(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
		next := args
		if providerOverride != "" {
			next.Provider = providerOverride
		}
		if modelOverride != "" {
			next.Model = modelOverride
		}
		resolved, err := Resolve(next, true)
		if err != nil {
			return nil, "", "", err
		}
		resolved.UseSandbox(sharedSandbox)
		resolved.MergeExtensionTools(extToolAdapter)
		injectSwarmSpawn(resolved.ToolRegistry)
		return wireAgentExt(resolved), resolved.Provider, resolved.Model, nil
	}

	// Rebuild agent for the rescue picker after a recoverable failure.
	// Unlike buildAgentFor, this drops launch-time --api-key and
	// --base-url overrides because those are typically the cause of the
	// rescue (a bad key, a typo'd base URL, or a corporate gateway that
	// only the originally-picked provider needed). Re-resolving without
	// them lets the rescue retry use env vars / auth.json / provider
	// defaults the way zot would have without the overrides.
	buildAgentForRescue := func(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
		next := args
		next.APIKey = ""
		next.BaseURL = ""
		if providerOverride != "" {
			next.Provider = providerOverride
		}
		if modelOverride != "" {
			next.Model = modelOverride
		}
		resolved, err := Resolve(next, true)
		if err != nil {
			return nil, "", "", err
		}
		resolved.UseSandbox(sharedSandbox)
		resolved.MergeExtensionTools(extToolAdapter)
		injectSwarmSpawn(resolved.ToolRegistry)
		return wireAgentExt(resolved), resolved.Provider, resolved.Model, nil
	}

	var ag *core.Agent
	if r.HasCredential() {
		ag = wireAgentExt(r)
	}

	// /reload-ext callback: after the manager has respawned every
	// extension, re-resolve the tool registry (built-ins + freshly-
	// registered extension tools) and swap it onto the current
	// agent in-place. The current agent may have been replaced by a
	// /model swap since spawn, so re-read the live `ag` on each
	// invocation.
	extMgr.SetOnReload(func() {
		current := liveInteractiveAgent(iv, ag)
		if current == nil {
			return
		}
		refreshAgentToolsAndPrompt(args, sharedSandbox, extToolAdapter, current, injectSwarmSpawn)
	})

	// Fire session_start once we know the manager's running.
	extMgr.EmitEvent(extproto.EventFromHost{Event: "session_start"})

	var sess *core.Session
	var sessBaselineMsgs int // messages already on disk when current session opened
	// persistMu guards sess + sessBaselineMsgs against concurrent access
	// from the agent loop's per-message persistence hook (runs on the
	// agent goroutine) and the TUI's session swap / flush callbacks
	// (run on the TUI goroutine). Without this, a /sessions swap that
	// races with a finishing turn could double-write or lose messages.
	var persistMu sync.Mutex
	if !args.NoSess && ag != nil {
		sess, _ = openOrCreateSession(args, r, ag, version)
		if ag != nil {
			sessBaselineMsgs = len(ag.Messages())
		}
	}
	defer func() {
		persistMu.Lock()
		defer persistMu.Unlock()
		if sess != nil {
			sess.Close()
		}
	}()

	// persistMessage is the per-message hook bound to the agent. It
	// appends each new transcript message to the live session as soon
	// as it lands, so a kill / closed terminal / OS crash costs at
	// most the in-flight turn instead of the whole session. The
	// baseline counter advances in lock-step so the exit-time flush
	// doesn't double-write rows already on disk.
	persistMessage := func(m provider.Message) {
		persistMu.Lock()
		defer persistMu.Unlock()
		if sess == nil {
			return
		}
		if err := sess.AppendMessage(m); err == nil {
			sessBaselineMsgs++
		}
	}
	persistUsage := func(cum provider.Usage) {
		persistMu.Lock()
		defer persistMu.Unlock()
		if sess == nil {
			return
		}
		_ = sess.AppendUsage(cum, cum)
	}
	persistCompaction := func(messages []provider.Message) {
		persistMu.Lock()
		defer persistMu.Unlock()
		if sess == nil {
			return
		}
		if err := sess.AppendCompaction(messages); err == nil {
			sessBaselineMsgs = len(messages)
		}
	}
	wireAgentPersist := func(a *core.Agent) *core.Agent {
		if a == nil {
			return a
		}
		a.OnMessageAppended = persistMessage
		a.OnUsage = persistUsage
		a.OnTranscriptCompacted = persistCompaction
		return a
	}
	wireAgentPersist(ag)

	// Re-wrap the build closures so any agent constructed by the TUI
	// (login, /model swap to a different provider) also gets the
	// persistence hooks. Without this, switching provider would
	// silently revert to the old in-memory-only behaviour.
	bindLiveAgentSession := func(a *core.Agent) {
		persistMu.Lock()
		s := sess
		persistMu.Unlock()
		bindAgentSession(a, s)
	}
	baseBuildAgent := buildAgent
	buildAgent = func() (*core.Agent, string, string, error) {
		a, p, m, err := baseBuildAgent()
		a = wireAgentPersist(a)
		bindLiveAgentSession(a)
		return a, p, m, err
	}
	baseBuildAgentFor := buildAgentFor
	buildAgentFor = func(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
		a, p, m, err := baseBuildAgentFor(providerOverride, modelOverride)
		a = wireAgentPersist(a)
		bindLiveAgentSession(a)
		return a, p, m, err
	}
	baseBuildAgentForRescue := buildAgentForRescue
	buildAgentForRescue = func(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
		a, p, m, err := baseBuildAgentForRescue(providerOverride, modelOverride)
		a = wireAgentPersist(a)
		bindLiveAgentSession(a)
		return a, p, m, err
	}

	// loadSession replaces the current session with the one at path and
	// hands its messages to the agent. Used by the /sessions picker.
	loadSession := func(path string) error {
		currentAg := liveInteractiveAgent(iv, ag)
		if currentAg == nil {
			return fmt.Errorf("no agent running; log in first")
		}
		newSess, msgs, err := core.OpenSession(path)
		if err != nil {
			return err
		}
		fullMsgCount := len(msgs)
		msgs = trimMessagesForResume(msgs, 100)
		persistMu.Lock()
		// Flush any unsaved messages to the old session before swapping.
		// Per-message persistence keeps sessBaselineMsgs current, so
		// this is a defensive no-op in the common case; it still
		// matters for the rare race where a turn just finished and
		// hadn't fired its hook yet.
		if sess != nil {
			writeNewTranscriptLocked(currentAg, sess, sessBaselineMsgs)
			_ = sess.Close()
		}
		sess = newSess
		currentAg.SetMessages(msgs)
		bindAgentSession(currentAg, sess)
		if cum, last, uerr := core.SessionUsageDetail(path); uerr == nil {
			currentAg.SeedCost(cum)
			currentAg.SeedLastTurnUsage(last)
		}
		// The live agent only receives a compact resume window, but
		// the session file remains intact. Keep the persistence
		// baseline at the original on-disk message count so future
		// turns append after the full session instead of duplicating
		// the hydrated tail.
		sessBaselineMsgs = fullMsgCount
		persistMu.Unlock()
		// Re-scope the swarm dashboard to the new session so /swarm
		// only shows agents this session spawned. swarmMgr may be nil
		// here if we haven't reached the construction site yet (it
		// shouldn't be, since the interactive loop is what triggers
		// loadSession, but the nil check is cheap insurance).
		if swarmMgr != nil && newSess != nil {
			swarmMgr.SetActiveSession(newSess.ID)
		}
		return nil
	}

	// changeCWD switches the running session to a new working directory.
	// Wired into InteractiveConfig.ChangeCWD and invoked by the hidden
	// /cd slash command (which itself is only fired by the workspaces
	// extension's panel-key Enter handler today; the user can type /cd
	// directly but it's not in autocomplete / help / the README).
	//
	// Steps, in order:
	//   1. resolve + validate the new path (~ expansion, abs/rel)
	//   2. close the current session, flushing pending messages
	//   3. mutate captured args.CWD + r.CWD so future buildAgent
	//      calls bind to the new cwd
	//   4. re-root the shared sandbox («·«) so /jail follows the
	//      session into the new cwd instead of widening or silently
	//      dropping
	//   5. rebuild the agent via buildAgent() so tools, AGENTS.md
	//      addendum, system prompt, sessions dir all bind correctly
	//   6. open a fresh session in the new cwd's bucket
	//   7. push the new state into the running Interactive
	//   8. re-scope the swarm dashboard to the freshly-opened session
	//
	// The /jail state is preserved verbatim: if the sandbox was locked
	// to the old cwd, it stays locked, just re-pointed at the new one.
	changeCWD := func(path string) error {
		if path == "" {
			return fmt.Errorf("empty path")
		}
		// ~ expansion.
		if path == "~" || strings.HasPrefix(path, "~/") {
			home, herr := os.UserHomeDir()
			if herr != nil || home == "" {
				return fmt.Errorf("cannot expand ~: %v", herr)
			}
			if path == "~" {
				path = home
			} else {
				path = filepath.Join(home, path[2:])
			}
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(args.CWD, path)
		}
		absPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(absPath)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("not a directory: %s", absPath)
		}

		currentAg := ag
		if currentAg == nil {
			return fmt.Errorf("no agent running; log in first")
		}

		// Close the current session before we drop the reference.
		// Per-message persistence keeps it current already; this is
		// a defensive flush + final fsync via Close.
		persistMu.Lock()
		if sess != nil {
			writeNewTranscriptLocked(currentAg, sess, sessBaselineMsgs)
			_ = sess.Close()
			sess = nil
		}
		sessBaselineMsgs = 0
		persistMu.Unlock()

		// Mutate captured state so subsequent agent rebuilds and
		// session opens see the new cwd.
		wasJailed := sharedSandbox != nil && sharedSandbox.Locked()
		args.CWD = absPath
		r.CWD = absPath
		if args.PermissionSet != nil {
			expanded := args.PermissionSet.Expand(absPath, args.AgentDataDir)
			args.PermissionSet = &expanded
		}
		if sharedSandbox != nil {
			sharedSandbox.Root = absPath
			if wasJailed {
				sharedSandbox.Lock()
			} else {
				sharedSandbox.Unlock()
			}
		}

		// Rebuild the agent so tools / AGENTS.md / system prompt
		// re-bind to the new cwd. buildAgent() reads from args + r.
		newAg, newProvider, newModel, berr := buildAgent()
		if berr != nil {
			return fmt.Errorf("rebuild agent: %v", berr)
		}
		ag = newAg

		// Fresh session in the new cwd's bucket. We bypass
		// openOrCreateSession's --continue / --resume branches
		// because /cd's semantics are "start fresh here", matching
		// what relaunching `zot --cwd <path>` would do today.
		if !args.NoSess {
			newRoot := agentSessionsRoot(ZotHome(), args)
			core.PruneEmptySessions(newRoot, absPath)
			newSess, serr := core.NewSession(newRoot, absPath, newProvider, newModel, version)
			if serr != nil {
				return fmt.Errorf("open session in %s: %v", absPath, serr)
			}
			persistMu.Lock()
			sess = newSess
			sessBaselineMsgs = 0
			persistMu.Unlock()
			bindAgentSession(newAg, newSess)
		}

		// Push the new state into the running Interactive.
		if iv != nil {
			startupPaths := instructionContextPaths(loadAgentsContext(absPath, ZotHome()))
			iv.ApplyChangedCWDWithStartupContext(newAg, newProvider, newModel, absPath, startupPaths)
		}

		// Re-scope the swarm dashboard to the new session.
		if swarmMgr != nil && sess != nil {
			swarmMgr.SetActiveSession(sess.ID)
		}
		return nil
	}

	term := tui.NewProcTerm()

	// Kick off the async update check so the banner can appear when the
	// http response eventually arrives (usually <1s on cached DNS). Map
	// agent.UpdateInfo -> modes.UpdateInfo here to avoid a cyclic import.
	updateCh := make(chan modes.UpdateInfo, 1)
	go func() {
		defer close(updateCh)
		src := <-CheckForUpdateAsync(ZotHome(), version)
		updateCh <- modes.UpdateInfo{
			Current:   src.Current,
			Latest:    src.Latest,
			Available: src.Available,
			URL:       src.URL,
		}
	}()

	// Changelog: when the running version differs from the last
	// version whose release notes the user dismissed, fetch the
	// release body from GitHub and have the TUI show it once. On
	// first-ever launch (no prior LastChangelogShown), seed the
	// stored version silently — don't dump release notes at someone
	// who just installed.
	changelogCh := make(chan modes.ChangelogPayload, 1)
	go func() {
		defer close(changelogCh)
		cfg, _ := LoadConfig()
		if cfg.LastChangelogShown == "" {
			SeedChangelogVersion(version)
			return
		}
		if !ShouldShowChangelog(version, cfg) {
			return
		}
		info := <-FetchChangelogAsync(version)
		if info.Body == "" {
			return
		}
		// For dev builds (0.0.0), skip if the latest release was
		// already shown (stored by the dismiss callback).
		if version == "0.0.0" && info.Version == cfg.LastChangelogShown {
			return
		}
		changelogCh <- modes.ChangelogPayload{
			Version: info.Version,
			Body:    info.Body,
			URL:     info.URL,
		}
	}()

	initialCfg, _ := LoadConfig()
	quickModelShortcuts := make([]modes.QuickModelShortcut, len(initialCfg.QuickModelShortcuts))
	for idx, s := range initialCfg.QuickModelShortcuts {
		quickModelShortcuts[idx] = modes.QuickModelShortcut{Provider: s.Provider, Model: s.Model}
	}
	theme, _, themeErr := tui.DetectThemeWithCustom(ZotHome(), initialCfg.Theme, 80*time.Millisecond)
	if themeErr != nil {
		fmt.Fprintln(os.Stderr, "theme load:", themeErr)
		if initialCfg.Theme != "" && !tui.ThemeExists(ZotHome(), initialCfg.Theme) {
			initialCfg.Theme = ""
			_ = SaveConfig(initialCfg)
		}
	}

	// swarmMgr was constructed and reloaded earlier (before the agent
	// build, so the auto-swarm tool could capture it). Here we just
	// scope the dashboard to the active host session so /swarm only
	// shows agents this session spawned (and any pre-upgrade unscoped
	// agents — see SnapshotAll docs). Updated again whenever the
	// user swaps sessions via loadSession below.
	if sess != nil {
		swarmMgr.SetActiveSession(sess.ID)
	}
	// Best-effort shutdown on interactive exit: stop all running
	// agents so they don't outlive their parent zot.
	defer swarmMgr.StopAll()

	var startupSkills []*skills.Skill
	if r.SkillTool != nil {
		startupSkills = r.SkillTool.Skills()
	}

	iv = modes.NewInteractive(modes.InteractiveConfig{
		Terminal:                      term,
		Theme:                         theme,
		InlineImagesEnabled:           initialCfg.InlineImagesEnabled,
		AutoSwarmEnabled:              initialCfg.AutoSwarmEnabled,
		AutoCompactThreshold:          initialCfg.AutoCompactThreshold,
		JailByDefault:                 initialCfg.JailByDefault,
		OpenRouterServerToolsEnabled:  initialCfg.OpenRouterServerToolsEnabled,
		OpenRouterServerToolCallLimit: initialCfg.OpenRouterServerToolCallLimit,
		NoTools:                       args.NoTools,
		QuickModelShortcuts:           quickModelShortcuts,
		RecursiveFileSuggest:          initialCfg.RecursiveFileSuggest,
		RespectGitignore:              initialCfg.RespectGitignore,
		CompactMode:                   initialCfg.CompactMode,
		CollapseToolCall:              initialCfg.CollapseToolCall,
		TUIInputStyle:                 initialCfg.TUIInputStyle,
		TUIStatusPosition:             initialCfg.TUIStatusPosition,
		TUIWorkingPosition:            initialCfg.TUIWorkingPosition,
		ThemeName:                     initialCfg.Theme,
		FlatTools:                     initialCfg.FlatToolRender(),
		CompactUser:                   initialCfg.CompactUserInput(),
		ExtensionThemes:               extMgr.ThemeOptions,
		AutoSwarmSystemAddendum:       AutoSwarmSystemAddendum,
		SettingsStore:                 configSettingsStore{},
		Model:                         r.Model,
		Provider:                      r.Provider,
		AuthMethod:                    r.AuthMethod,
		BaseURL:                       r.BaseURL,
		Reasoning:                     r.Reasoning,
		SystemPrompt:                  r.SystemPrompt,
		Tools:                         r.ToolRegistry,
		MaxSteps:                      r.MaxSteps,
		CWD:                           r.CWD,
		StartupAgentName:              args.AgentName,
		StartupContextPaths:           instructionContextPaths(r.ContextFiles),
		StartupExtensionNames:         startupExtensionNames(extMgr.All()),
		StartupExtensionErrors:        startupExtensionErrors,
		StartupSkillNames:             startupSkillNames(startupSkills),
		ShowInstructionsAtStartup:     initialCfg.ShowInstructionsAtStartup,
		ZotHome:                       ZotHome(),
		SessionsRoot:                  agentSessionsRoot(ZotHome(), args),
		Version:                       version,
		UpdateInfoChan:                updateCh,
		Sandbox:                       sharedSandbox,
		Agent:                         ag,
		InitialInput:                  args.Prompt,
		StartupPre:                    args.StartupPre,
		OnStartupPreDone: func() {
			// entry.pre often installs skills/extensions; rediscover them
			// before the user starts a model turn.
			current := liveInteractiveAgent(iv, ag)
			if extMgr != nil {
				_ = extMgr.Reload(context.Background(), 2*time.Second)
			}
			if current != nil {
				refreshAgentToolsAndPrompt(args, sharedSandbox, extToolAdapter, current, injectSwarmSpawn)
			}
		},
		AuthManager:                mgr,
		LlamaCPPConfig:             ResolveLlamaCPPConfig,
		RefreshLlamaCPPModels:      RefreshLlamaCPPModels,
		BuildAgent:                 buildAgent,
		SetKimiCLIFallbackDisabled: SetKimiCLIFallbackDisabled,
		BuildAgentFor:              buildAgentFor,
		BuildAgentForRescue:        buildAgentForRescue,
		LoggedInProviders: func() []string {
			var out []string
			seen := map[string]bool{}
			for _, p := range knownProviders {
				if CredentialAvailable(p) && !seen[p] {
					out = append(out, p)
					seen[p] = true
				}
			}
			// Include custom providers that have credentials stored.
			for p := range provider.CustomProviders() {
				if CredentialAvailable(p) && !seen[p] {
					out = append(out, p)
					seen[p] = true
				}
			}
			// Ollama models are always available (no auth needed).
			if !seen["ollama"] {
				out = append(out, "ollama")
			}
			return out
		},
		LoadSession: loadSession,
		ChangeCWD:   changeCWD,
		CurrentSessionPath: func() string {
			if sess == nil {
				return ""
			}
			return sess.Path
		},
		FlushSession: func() {
			// Append any not-yet-persisted agent messages to the
			// current session file, then advance the baseline so
			// the final WriteNewTranscript at exit doesn't write
			// duplicates. Per-message persistence keeps the on-
			// disk file current already, so this is mostly a
			// defensive flush — still needed for /session export
			// to guarantee the exported bytes include the very
			// last in-flight turn.
			currentAg := iv.Agent()
			if currentAg == nil {
				return
			}
			persistMu.Lock()
			defer persistMu.Unlock()
			if sess == nil {
				return
			}
			writeNewTranscriptLocked(currentAg, sess, sessBaselineMsgs)
			sessBaselineMsgs = len(currentAg.Messages())
		},
		Extensions:    extMgr,
		Swarm:         swarmMgr,
		ChangelogChan: changelogCh,
		OnChangelogDismiss: func() {
			// For dev builds (0.0.0) store the actual release version
			// so the same changelog doesn't show again next launch.
			// For real builds, store the binary version.
			v := version
			if v == "0.0.0" {
				if iv != nil && iv.ChangelogVersion() != "" {
					v = iv.ChangelogVersion()
				}
			}
			_ = MarkChangelogShown(v)
		},
		SkillSnapshot: func() []*skills.Skill {
			if args.NoSkill {
				// --no-skill: nothing for the picker to show.
				return nil
			}
			// Re-discover so the picker reflects edits made during
			// the session. Cheap; SKILL.md files are small. Filter
			// out built-in skills — they're hidden from user-facing
			// surfaces because they're implementation detail; the
			// model still sees them through the system-prompt
			// manifest and the skill tool.
			userHome, _ := os.UserHomeDir()
			var sources []skills.Source
			if args.WithSkills {
				sources = append(sources, skills.SearchSources(ZotHome(), r.CWD, userHome)...)
			}
			if !args.NoExt || len(args.Exts) > 0 {
				extSources, _ := extensions.PlanSkillSources(ZotHome(), r.CWD, args.Exts, !args.NoExt)
				sources = append(extSources, sources...)
			}
			list, _ := skills.DiscoverSources(sources, true)
			return skills.VisibleSkills(list)
		},
		NoYolo:      args.NoYolo,
		ConfirmGate: confirmGate,
		PersistModel: func(providerName, model string) {
			// Update config.json so next launch uses the same pick.
			cfg, _ := LoadConfig()
			cfg.Provider = providerName
			cfg.Model = model
			_ = SaveConfig(cfg)
			// Update the active session's meta so resume picks this up.
			if sess != nil {
				_ = sess.UpdateModel(providerName, model)
			}
		},
	})

	// Bind the interactive TUI as the Confirmer. We deferred this
	// until now because the gate is constructed before the TUI
	// (the BeforeToolExecute closure captures it). SetConfirmer
	// is mutex-guarded on the gate so this is safe.
	if confirmGate != nil {
		confirmGate.SetConfirmer(&confirmationEventConfirmer{
			inner: iv,
			emit:  extMgr.EmitEvent,
		})
	}

	// Signal-driven flush: a SIGTERM / SIGHUP to the zot process
	// (closed terminal window, system shutdown, kill) used to lose
	// the entire in-memory transcript because the deferred post-Run
	// flush below never ran. Per-message persistence above covers
	// most of it; this handler writes any in-flight remainder and
	// then exits the process so we don't double-paint over a
	// broken terminal that the TUI's restore deferreds can no
	// longer fix from a signal context.
	//
	// SIGINT is intentionally NOT handled here — the TUI consumes
	// Ctrl+C as a regular key event for cancel/clear semantics, and
	// installing a SIGINT notifier here would swallow it.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigCh)
	go func() {
		_, ok := <-sigCh
		if !ok {
			return
		}
		if finalAg := iv.Agent(); finalAg != nil {
			persistMu.Lock()
			if sess != nil {
				writeNewTranscriptLocked(finalAg, sess, sessBaselineMsgs)
				sessBaselineMsgs = len(finalAg.Messages())
				_ = sess.Close()
				sess = nil
			}
			persistMu.Unlock()
		}
		// Exit cleanly. Re-raising the signal would skip os.Exit's
		// at-exit hooks; explicit exit is fine because we've already
		// flushed the only at-risk state (the session file).
		os.Exit(0)
	}()

	runErr := iv.Run(ctx)

	// Flush final transcript to session (only if we had / ended up with an agent).
	if finalAg := iv.Agent(); finalAg != nil {
		persistMu.Lock()
		if sess != nil {
			writeNewTranscriptLocked(finalAg, sess, sessBaselineMsgs)
			sessBaselineMsgs = len(finalAg.Messages())
		}
		persistMu.Unlock()
	}
	return runErr
}

func agentSessionsRoot(root string, args Args) string {
	if args.AgentName == "" {
		return root
	}
	return filepath.Join(root, "sessions", "agents", safeAgentName(args.AgentName))
}

// openOrCreateSession returns a session for the run. sess may be nil
// with a nil error if session persistence is disabled. When a session
// exists, its id is bound onto ag so providers can sticky-route.
func openOrCreateSession(args Args, r Resolved, ag *core.Agent, version string) (*core.Session, error) {
	if args.NoSess {
		return nil, nil
	}
	// Sweep meta-only files left over from older zot versions (and from
	// any session that crashed before its first AppendMessage). Cheap;
	// reads the first few bytes of each file in the cwd's session dir.
	sessionsRoot := agentSessionsRoot(ZotHome(), args)
	core.PruneEmptySessions(sessionsRoot, args.CWD)
	var (
		s    *core.Session
		msgs []provider.Message
		err  error
	)
	switch {
	case args.Session != "":
		s, msgs, err = core.OpenSession(args.Session)
		// The swarm-agent child passes a fixed --session path that
		// may not exist yet on first Spawn. Treat ENOENT as "create
		// a fresh session AT THIS PATH" so the conversation actually
		// gets persisted; without this fallback the swarm child runs
		// with sess==nil and every Resume re-starts with no memory
		// of the prior turns. Other openers (--continue / --resume /
		// the picker) never see ENOENT here because they only choose
		// paths that already exist on disk.
		if err != nil && errors.Is(err, os.ErrNotExist) {
			s, err = core.NewSessionAtPath(args.Session, args.CWD, r.Provider, r.Model, version)
			msgs = nil
		}
	case args.Continue:
		latest := core.LatestSession(sessionsRoot, args.CWD)
		if latest != "" {
			s, msgs, err = core.OpenSession(latest)
		}
	case args.Resume:
		picked, perr := pickSession(sessionsRoot, args.CWD)
		if perr != nil {
			return nil, perr
		}
		if picked != "" {
			s, msgs, err = core.OpenSession(picked)
		}
	}
	if err != nil {
		return nil, err
	}
	if s == nil {
		s, err = core.NewSession(sessionsRoot, args.CWD, r.Provider, r.Model, version)
		if err != nil {
			return nil, err
		}
	} else {
		ag.SetMessages(msgs)
		if cum, last, uerr := core.SessionUsageDetail(s.Path); uerr == nil {
			ag.SeedCost(cum)
			ag.SeedLastTurnUsage(last)
		}
	}
	bindAgentSession(ag, s)
	return s, nil
}

func pickSession(root, cwd string) (string, error) {
	files := core.ListSessions(root, cwd)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions for", cwd)
		return "", nil
	}
	for i, f := range files {
		fmt.Fprintf(os.Stderr, "  %2d) %s\n", i+1, f)
	}
	fmt.Fprint(os.Stderr, "pick #: ")
	rd := bufio.NewReader(os.Stdin)
	line, _ := rd.ReadString('\n')
	line = strings.TrimSpace(line)
	var n int
	if _, err := fmt.Sscanf(line, "%d", &n); err != nil || n < 1 || n > len(files) {
		return "", fmt.Errorf("invalid selection")
	}
	return files[n-1], nil
}

// WriteNewTranscript appends only messages after index `from` from the
// agent's transcript to the session. Used by callers that don't hold
// the persistMu (non-interactive print/json modes which run a single
// turn under their own goroutine).
func WriteNewTranscript(ag *core.Agent, sess *core.Session, from int) {
	writeNewTranscriptLocked(ag, sess, from)
}

// bindAgentSession stamps the zot session id onto the agent so providers
// that support sticky routing (OpenRouter session_id) can reuse it.
// No-op when persistence is disabled.
func bindAgentSession(ag *core.Agent, sess *core.Session) {
	if ag == nil || sess == nil {
		return
	}
	ag.SessionID = sess.ID
}

// writeNewTranscriptLocked is the same as WriteNewTranscript. The
// suffix marks that interactive callers must hold persistMu when
// invoking it so concurrent appends from the agent loop don't race
// with this catch-up flush.
func writeNewTranscriptLocked(ag *core.Agent, sess *core.Session, from int) {
	if sess == nil || ag == nil {
		return
	}
	msgs := ag.Messages()
	for i := from; i < len(msgs); i++ {
		_ = sess.AppendMessage(msgs[i])
	}
	cum := ag.Cost()
	_ = sess.AppendUsage(cum, cum)
}

func argsForStdin(rawArgs []string, mode os.FileMode) []string {
	if mode&os.ModeCharDevice != 0 {
		return rawArgs
	}
	return append([]string{"--print"}, rawArgs...)
}

func combinePromptInput(stdin, positional string) string {
	parts := make([]string, 0, 2)
	if stdin = strings.TrimSpace(stdin); stdin != "" {
		parts = append(parts, stdin)
	}
	if positional = strings.TrimSpace(positional); positional != "" {
		parts = append(parts, positional)
	}
	return strings.Join(parts, "\n")
}

func readAllStdin() (string, error) {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return "", err
	}
	if (fi.Mode() & os.ModeCharDevice) != 0 {
		return "", nil
	}
	b, err := io.ReadAll(os.Stdin)
	return string(b), err
}

func printModels() {
	models := provider.Active()

	// Compute column widths from actual data so wide providers (e.g.
	// xiaomi-token-plan-sgp) and long bedrock model ids don't force the
	// `name` column off-screen. Floors mirror the historical layout so
	// short catalogs look the same as before.
	provW, idW, srcW := len("provider"), len("model id"), len("source")
	for _, m := range models {
		if w := len(m.Provider); w > provW {
			provW = w
		}
		if w := len(m.ID); w > idW {
			idW = w
		}
		source := m.Source
		if source == "" {
			source = "catalog"
		}
		if m.Speculative {
			source = "speculative"
		}
		if w := len(source); w > srcW {
			srcW = w
		}
	}

	header := fmt.Sprintf("%-*s  %-*s  %8s  %8s  %s  %-*s  %s",
		provW, "provider",
		idW, "model id",
		"context", "max-out", "reasoning",
		srcW, "source",
		"name")
	fmt.Println(header)

	for _, m := range models {
		reason := " "
		if m.Reasoning {
			reason = "✓"
		}
		source := m.Source
		if source == "" {
			source = "catalog"
		}
		if m.Speculative {
			source = "speculative"
		}
		fmt.Printf("%-*s  %-*s  %8d  %8d     %s      %-*s  %s\n",
			provW, m.Provider,
			idW, m.ID,
			m.ContextWindow, m.MaxOutput,
			reason,
			srcW, source,
			m.DisplayName)
	}
}
