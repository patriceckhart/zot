package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/patriceckhart/zot/internal/agent/extensions"
	"github.com/patriceckhart/zot/internal/agent/modes"
	"github.com/patriceckhart/zot/internal/auth"
	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/extproto"
	"github.com/patriceckhart/zot/internal/provider"
	"github.com/patriceckhart/zot/internal/skills"
	"github.com/patriceckhart/zot/internal/tui"
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
		iv.Submit(text)
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
	})
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
	// Subcommand router: `zot bot ...` is handled separately so the
	// generic flag parser doesn't reject "bot" as a positional arg.
	if handled, err := runBotCommand(rawArgs, version); handled {
		return err
	}
	if handled, err := runExtCommand(rawArgs); handled {
		return err
	}
	// `zot rpc` is shorthand for `zot --rpc` so third-party apps can
	// spawn the binary with a clean argv. Strip the leading 'rpc'
	// token and let the rest flow through the normal arg parser.
	if len(rawArgs) > 0 && rawArgs[0] == "rpc" {
		rawArgs = append([]string{"--rpc"}, rawArgs[1:]...)
	}

	args, err := ParseArgs(rawArgs)
	if err != nil {
		PrintHelp(version)
		return err
	}
	if args.Help {
		PrintHelp(version)
		return nil
	}
	if args.Version {
		fmt.Println("zot", version)
		return nil
	}
	// Model catalog: load any cached discovery data before we inspect
	// the model list (list-models, print/json, interactive).
	LoadCachedModels()
	LoadUserModels()

	if args.ListModels {
		printModels()
		return nil
	}

	ctx := context.Background()

	// Kick an async refresh of the live model catalog. The first run of
	// zot hits the network; subsequent runs within CacheTTL do nothing.
	RefreshModelsAsync()

	switch args.Mode {
	case ModePrint:
		return runPrintMode(ctx, args, version)
	case ModeJSON:
		return runJSONMode(ctx, args, version)
	case ModeRPC:
		return runRPCMode(ctx, args, version)
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
func (nonInteractiveExtHooks) Insert(string)                                        {}
func (nonInteractiveExtHooks) Display(string, string)                               {}
func (nonInteractiveExtHooks) OpenPanel(string, extproto.PanelSpec)                 {}
func (nonInteractiveExtHooks) UpdatePanel(string, string, string, []string, string) {}
func (nonInteractiveExtHooks) ClosePanel(string, string)                            {}

// setupNonInteractiveExtensions loads --ext paths and (unless
// --no-ext) runs discovery. Returns the manager so the caller can
// wire tools into the resolved registry, and a cleanup closure to
// defer. Mirrors the interactive-mode setup minus the TUI hooks.
func setupNonInteractiveExtensions(ctx context.Context, args Args, r *Resolved, version string) (*extensions.Manager, func()) {
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

// wireNonInteractiveAgentExtHooks installs the same BeforeToolExecute
// / BeforeTurn / BeforeAssistantMessage / OnEvent hooks the
// interactive path wires up, so extensions get their normal
// event-intercept surface in print / json / rpc flows too.
func wireNonInteractiveAgentExtHooks(ctx context.Context, ag *core.Agent, extMgr *extensions.Manager) {
	if ag == nil || extMgr == nil {
		return
	}
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

	prompt := args.Prompt
	if prompt == "" {
		piped, _ := readAllStdin()
		prompt = strings.TrimSpace(piped)
	}
	if prompt == "" {
		return fmt.Errorf("print mode requires a prompt (arg or stdin)")
	}

	start := len(ag.Messages())
	err = modes.RunPrint(ctx, ag, prompt, nil, os.Stdout)
	WriteNewTranscript(ag, sess, start)
	return err
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

	prompt := args.Prompt
	if prompt == "" {
		piped, _ := readAllStdin()
		prompt = strings.TrimSpace(piped)
	}
	if prompt == "" {
		return fmt.Errorf("json mode requires a prompt (arg or stdin)")
	}

	start := len(ag.Messages())
	err = modes.RunJSON(ctx, ag, prompt, nil, os.Stdout)
	WriteNewTranscript(ag, sess, start)
	return err
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
	// --ext paths first so they win against installed extensions of
	// the same name (loadOne's first-write-wins semantics).
	for _, e := range extMgr.LoadExplicit(ctx, args.Exts) {
		fmt.Fprintln(os.Stderr, "extension load:", e)
	}
	// --no-ext skips the global + project-local discovery scan;
	// explicit --ext paths above are still honoured so you can run
	// "only this extension" with --no-ext --ext ./x.
	if !args.NoExt {
		for _, e := range extMgr.Discover(ctx) {
			fmt.Fprintln(os.Stderr, "extension load:", e)
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
	wireAgentExt := func(a *core.Agent) *core.Agent {
		if a == nil {
			return a
		}
		a.BeforeToolExecute = func(call provider.ToolCallBlock) (bool, string, json.RawMessage) {
			// Confirm gate runs FIRST: if the user refused, we don't
			// waste extension-intercept time or let guards see the call.
			if confirmGate != nil {
				ok, reason, _ := confirmGate.Check(call.Name, core.BuildPreview(call.Arguments, 120))
				if !ok {
					return false, reason, nil
				}
			}
			r := extMgr.InterceptToolCall(ctx, call.ID, call.Name, call.Arguments)
			if r.Block {
				return false, r.Reason, nil
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
		return wireAgentExt(resolved.NewAgent()), resolved.Provider, resolved.Model, nil
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
		return wireAgentExt(resolved.NewAgent()), resolved.Provider, resolved.Model, nil
	}

	var ag *core.Agent
	if r.HasCredential() {
		ag = wireAgentExt(r.NewAgent())
	}

	// /reload-ext callback: after the manager has respawned every
	// extension, re-resolve the tool registry (built-ins + freshly-
	// registered extension tools) and swap it onto the current
	// agent in-place. The current agent may have been replaced by a
	// /model swap since spawn, so re-read the live `ag` on each
	// invocation.
	extMgr.SetOnReload(func() {
		current := ag
		if current == nil {
			return
		}
		resolved, err := Resolve(args, true)
		if err != nil {
			return
		}
		resolved.UseSandbox(sharedSandbox)
		resolved.MergeExtensionTools(extToolAdapter)
		current.SetTools(resolved.ToolRegistry)
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
	wireAgentPersist := func(a *core.Agent) *core.Agent {
		if a == nil {
			return a
		}
		a.OnMessageAppended = persistMessage
		a.OnUsage = persistUsage
		return a
	}
	wireAgentPersist(ag)

	// Re-wrap the build closures so any agent constructed by the TUI
	// (login, /model swap to a different provider) also gets the
	// persistence hooks. Without this, switching provider would
	// silently revert to the old in-memory-only behaviour.
	baseBuildAgent := buildAgent
	buildAgent = func() (*core.Agent, string, string, error) {
		a, p, m, err := baseBuildAgent()
		return wireAgentPersist(a), p, m, err
	}
	baseBuildAgentFor := buildAgentFor
	buildAgentFor = func(providerOverride, modelOverride string) (*core.Agent, string, string, error) {
		a, p, m, err := baseBuildAgentFor(providerOverride, modelOverride)
		return wireAgentPersist(a), p, m, err
	}

	// loadSession replaces the current session with the one at path and
	// hands its messages to the agent. Used by the /sessions picker.
	loadSession := func(path string) error {
		currentAg := ag // captured
		if currentAg == nil {
			return fmt.Errorf("no agent running; log in first")
		}
		newSess, msgs, err := core.OpenSession(path)
		if err != nil {
			return err
		}
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
		if usage, uerr := core.SessionUsage(path); uerr == nil {
			currentAg.SeedCost(usage)
		}
		sessBaselineMsgs = len(msgs)
		persistMu.Unlock()
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

	iv = modes.NewInteractive(modes.InteractiveConfig{
		Terminal:       term,
		Theme:          tui.DetectThemeFromBackground(80 * time.Millisecond),
		Model:          r.Model,
		Provider:       r.Provider,
		AuthMethod:     r.AuthMethod,
		BaseURL:        r.BaseURL,
		Reasoning:      r.Reasoning,
		SystemPrompt:   r.SystemPrompt,
		Tools:          r.ToolRegistry,
		MaxSteps:       r.MaxSteps,
		CWD:            r.CWD,
		ZotHome:        ZotHome(),
		Version:        version,
		UpdateInfoChan: updateCh,
		Sandbox:        sharedSandbox,
		Agent:          ag,
		InitialInput:   args.Prompt,
		AuthManager:    mgr,
		BuildAgent:     buildAgent,
		BuildAgentFor:  buildAgentFor,
		LoggedInProviders: func() []string {
			var out []string
			for _, p := range []string{"anthropic", "openai"} {
				if _, _, err := ResolveCredential(p, ""); err == nil {
					out = append(out, p)
				}
			}
			// Ollama models are always available (no auth needed).
			out = append(out, "ollama")
			return out
		},
		LoadSession: loadSession,
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
			// manifest and the skill tool. User skills only appear
			// when --with-skills is set; without it the picker shows
			// nothing but the model still has the built-ins.
			userHome, _ := os.UserHomeDir()
			list, _ := skills.Discover(ZotHome(), r.CWD, userHome, args.WithSkills)
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
		confirmGate.SetConfirmer(iv)
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

// openOrCreateSession returns a session for the run. sess may be nil
// with a nil error if session persistence is disabled.
func openOrCreateSession(args Args, r Resolved, ag *core.Agent, version string) (*core.Session, error) {
	if args.NoSess {
		return nil, nil
	}
	// Sweep meta-only files left over from older zot versions (and from
	// any session that crashed before its first AppendMessage). Cheap;
	// reads the first few bytes of each file in the cwd's session dir.
	core.PruneEmptySessions(ZotHome(), args.CWD)
	var (
		s    *core.Session
		msgs []provider.Message
		err  error
	)
	switch {
	case args.Session != "":
		s, msgs, err = core.OpenSession(args.Session)
	case args.Continue:
		latest := core.LatestSession(ZotHome(), args.CWD)
		if latest != "" {
			s, msgs, err = core.OpenSession(latest)
		}
	case args.Resume:
		picked, perr := pickSession(args.CWD)
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
	if s != nil {
		ag.SetMessages(msgs)
		if usage, uerr := core.SessionUsage(s.Path); uerr == nil {
			ag.SeedCost(usage)
		}
		return s, nil
	}
	return core.NewSession(ZotHome(), args.CWD, r.Provider, r.Model, version)
}

func pickSession(cwd string) (string, error) {
	files := core.ListSessions(ZotHome(), cwd)
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
	fmt.Println("provider   model id                       context  max-out  reasoning  source        name")
	for _, m := range provider.Active() {
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
		fmt.Printf("%-10s %-30s %8d %8d     %s        %-11s   %s\n",
			m.Provider, m.ID, m.ContextWindow, m.MaxOutput, reason, source, m.DisplayName)
	}
}
