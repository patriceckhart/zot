package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/patriceckhart/zot/internal/agent/extensions"
	"github.com/patriceckhart/zot/internal/agent/modes"
	"github.com/patriceckhart/zot/internal/auth"
	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
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

func runPrintMode(ctx context.Context, args Args, version string) error {
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	ag := r.NewAgent()
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
	r, err := Resolve(args, true)
	if err != nil {
		return err
	}
	ag := r.NewAgent()
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

	// Capture current args in a closure so BuildAgent can re-resolve
	// after a successful login (picks up the newly stored credential).
	buildAgent := func() (*core.Agent, string, string, error) {
		resolved, err := Resolve(args, true)
		if err != nil {
			return nil, "", "", err
		}
		resolved.UseSandbox(sharedSandbox)
		return resolved.NewAgent(), resolved.Provider, resolved.Model, nil
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
		return resolved.NewAgent(), resolved.Provider, resolved.Model, nil
	}

	var ag *core.Agent
	if r.HasCredential() {
		ag = r.NewAgent()
	}

	var sess *core.Session
	var sessBaselineMsgs int // messages already on disk when current session opened
	if !args.NoSess && ag != nil {
		sess, _ = openOrCreateSession(args, r, ag, version)
		if ag != nil {
			sessBaselineMsgs = len(ag.Messages())
		}
	}
	defer func() {
		if sess != nil {
			sess.Close()
		}
	}()

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
		// Flush any unsaved messages to the old session before swapping.
		if sess != nil {
			WriteNewTranscript(currentAg, sess, sessBaselineMsgs)
			_ = sess.Close()
		}
		sess = newSess
		currentAg.SetMessages(msgs)
		sessBaselineMsgs = len(msgs)
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

	// Forward-declare iv so the extension manager hook adapter can
	// dereference it after construction. The manager is started after
	// iv is built so iv (which implements extensions.HostHooks) is
	// available as the hook target.
	var iv *modes.Interactive
	extHooks := &interactiveExtHooks{ivPtr: &iv}
	extMgr := extensions.New(ZotHome(), r.CWD, version, r.Provider, r.Model, extHooks)
	discoveryErrs := extMgr.Discover(ctx)
	for _, e := range discoveryErrs {
		fmt.Fprintln(os.Stderr, "extension load:", e)
	}
	defer extMgr.Stop(2 * time.Second)

	iv = modes.NewInteractive(modes.InteractiveConfig{
		Terminal:       term,
		Theme:          tui.Dark,
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
		LoadSession:    loadSession,
		Extensions:     extMgr,
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

	runErr := iv.Run(ctx)

	// Flush final transcript to session (only if we had / ended up with an agent).
	if finalAg := iv.Agent(); finalAg != nil && sess != nil {
		WriteNewTranscript(finalAg, sess, sessBaselineMsgs)
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
// agent's transcript to the session.
func WriteNewTranscript(ag *core.Agent, sess *core.Session, from int) {
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
