package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// continuousRunOptions are the execution-specific flags of `zot continuous run`.
type continuousRunOptions struct {
	store      string
	backend    string
	durability journal.Options
	workspace  string
	prompt     string
	requestID  string
	resumeOnly bool
	json       bool
	maxTurns   int
	// hostArgs are ordinary zot flags (provider, model, tools, cwd, ext) that
	// decide how the engine is built. They are parsed by ParseArgs.
	hostArgs []string
}

func parseContinuousRunArgs(args []string) (continuousRunOptions, error) {
	opts := continuousRunOptions{}
	positionalOnly := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if positionalOnly || !strings.HasPrefix(arg, "-") {
			if opts.prompt != "" {
				return opts, fmt.Errorf("continuous run accepts one prompt")
			}
			opts.prompt = arg
			continue
		}
		if arg == "--" {
			positionalOnly = true
			continue
		}
		value := func() (string, error) {
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "--store":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.store = v
		case "--durability":
			v, err := value()
			if err != nil {
				return opts, err
			}
			switch v {
			case "strict", "process":
				opts.durability.Durability = continuousDurability(v)
			default:
				return opts, fmt.Errorf("durability must be strict or process")
			}
		case "--backend":
			v, err := value()
			if err != nil {
				return opts, err
			}
			if !validContinuousBackend(v) {
				return opts, fmt.Errorf("backend must be journal or sqlite")
			}
			opts.backend = v
		case "--workspace":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.workspace = v
		case "--request-id":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.requestID = v
		case "--max-turns":
			v, err := value()
			if err != nil {
				return opts, err
			}
			if _, err := fmt.Sscanf(v, "%d", &opts.maxTurns); err != nil || opts.maxTurns < 1 {
				return opts, fmt.Errorf("--max-turns requires a positive integer")
			}
		case "--resume":
			opts.resumeOnly = true
		case "--json":
			opts.json = true
		case "-p", "--print", "--stream", "--rpc", "-c", "--continue", "-r", "--session", "--no-session", "--swarm-agent", "--list-models", "--stats":
			// Session files and other host modes are not the authority here.
			return opts, fmt.Errorf("%s does not apply to continuous run", arg)
		default:
			// Everything else is a host flag. Flags with values consume the
			// next argument when it does not look like a flag.
			opts.hostArgs = append(opts.hostArgs, arg)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && hostFlagTakesValue(arg) {
				i++
				opts.hostArgs = append(opts.hostArgs, args[i])
			}
		}
	}
	if opts.store == "" {
		return opts, fmt.Errorf("continuous run requires --store")
	}
	if opts.prompt == "" && !opts.resumeOnly {
		return opts, fmt.Errorf("continuous run requires a prompt or --resume")
	}
	if opts.prompt != "" && opts.resumeOnly {
		return opts, fmt.Errorf("--resume does not take a prompt")
	}
	return opts, nil
}

// hostFlagTakesValue lists the ordinary zot flags with a value that
// continuous run forwards. Boolean host flags are forwarded alone.
func hostFlagTakesValue(flag string) bool {
	switch flag {
	case "--provider", "--model", "--api-key", "--base-url", "--system-prompt", "--append-system-prompt", "--reasoning", "--temperature", "--cwd", "--tools", "--ext", "-e", "--with-skills", "--with-skill":
		return true
	}
	return false
}

// signalContext cancels on interrupt so an in-flight tool or request ends
// cleanly. The run stays recoverable: committed intent is never lost.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// runContinuousRun executes queued work for one workspace root conversation
// with the host's ordinary provider, tool, and extension configuration. The
// engine is rebuilt per model request from committed state; the durable
// service decides what runs. Credentials are resolved by the host only.
func runContinuousRun(ctx context.Context, args []string, out io.Writer) (retErr error) {
	opts, err := parseContinuousRunArgs(args)
	if err != nil {
		return err
	}
	hostArgs, err := ParseArgs(append([]string{"--print"}, opts.hostArgs...))
	if err != nil {
		return err
	}
	r, err := Resolve(hostArgs, true)
	if err != nil {
		return err
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, hostArgs, &r, "continuous")
	defer stopExt()
	store, err := openContinuousStore(ctx, opts.store, opts.backend, opts.durability.Durability)
	if err != nil {
		return err
	}
	rt, err := continuous.New(store)
	if err != nil {
		store.Close()
		return err
	}
	defer func() { retErr = errors.Join(retErr, rt.Close()) }()
	workspace := opts.workspace
	if workspace == "" {
		workspace = r.CWD
	}
	root, err := rt.OpenRoot(ctx, workspace, continuous.AgentConfig{Provider: r.Provider, Model: r.Model, Reasoning: r.Reasoning})
	if err != nil {
		return err
	}
	var svc *continuous.Service
	engine := continuous.EngineFunc(func(ctx context.Context, c continuous.Conversation) (*core.Agent, error) {
		ag := r.NewAgent()
		wireNonInteractiveAgentExtHooks(ctx, ag, extMgr)
		if svc != nil {
			ag.Tools["subagent"] = &continuous.SubagentTool{Runtime: rt, Service: svc}
		}
		ag.Tools["handoff"] = continuous.HandoffTool{}
		return ag, nil
	})
	enc := json.NewEncoder(out)
	var text strings.Builder
	sink := func(ev core.AgentEvent) {
		if opts.json {
			if row, ok := continuousEventRow(ev); ok {
				_ = enc.Encode(row)
			}
			return
		}
		switch e := ev.(type) {
		case core.EvTextDelta:
			text.WriteString(e.Delta)
		case core.EvAssistantMessage:
			text.Reset()
			text.WriteString(core.MessageText(e.Message))
		case core.EvToolCall:
			fmt.Fprintf(os.Stderr, "tool %s %s\n", e.Name, string(e.Args))
		case core.EvToolResult:
			fmt.Fprintf(os.Stderr, "tool %s %s\n", e.Name, e.Status)
		}
	}
	svc, err = continuous.NewService(rt, engine, continuous.ExecutionOptions{MaxTurns: opts.maxTurns, Sink: sink, Compaction: continuousCompaction(r)})
	if err != nil {
		return err
	}
	var submission continuous.Submission
	if opts.prompt != "" {
		submission, err = rt.Submit(ctx, root.ID, "cli:"+userActor(), opts.requestID, opts.prompt)
		if err != nil {
			return err
		}
	}
	run, did, err := svc.Step(ctx, root.ID)
	if opts.json {
		_ = enc.Encode(struct {
			Type         string                 `json:"type"`
			Conversation string                 `json:"conversation"`
			Run          *continuous.Run        `json:"run,omitempty"`
			Submission   *continuous.Submission `json:"submission,omitempty"`
			Error        string                 `json:"error,omitempty"`
		}{"run_end", root.ID, runPtr(run, did), submissionPtr(rt, ctx, submission), errString(err)})
		return err
	}
	if err != nil {
		return err
	}
	if !did {
		fmt.Fprintln(os.Stderr, "nothing queued, nothing to recover")
		return nil
	}
	for _, notice := range run.Notices {
		fmt.Fprintln(os.Stderr, "recovery:", notice)
	}
	if run.Outcome != "completed" {
		return fmt.Errorf("run %s: %s", run.Outcome, run.Error)
	}
	if text.Len() > 0 {
		fmt.Fprintln(out, text.String())
	}
	return nil
}

func runPtr(run continuous.Run, did bool) *continuous.Run {
	if !did && run.ID == "" {
		return nil
	}
	return &run
}

func submissionPtr(rt *continuous.Runtime, ctx context.Context, s continuous.Submission) *continuous.Submission {
	if s.ID == "" {
		return nil
	}
	if current, err := rt.Submission(ctx, s.ID); err == nil {
		return &current
	}
	return &s
}

func userActor() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u := os.Getenv("USERNAME"); u != "" {
		return u
	}
	return "local"
}

// continuousEventRow projects engine events into a flat JSON line. Tool
// results carry text only; images and structured content are omitted here.
func continuousEventRow(ev core.AgentEvent) (any, bool) {
	switch e := ev.(type) {
	case core.EvTextDelta:
		return struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{"text", e.Delta}, true
	case core.EvToolCall:
		return struct {
			Type string          `json:"type"`
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}{"tool_call", e.ID, e.Name, e.Args}, true
	case core.EvToolResult:
		return struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Name    string `json:"name"`
			Status  string `json:"status"`
			IsError bool   `json:"is_error"`
			Text    string `json:"text"`
		}{"tool_result", e.ID, e.Name, e.Status, e.Result.IsError, core.ToolResultText(e.Result)}, true
	case core.EvUsage:
		return struct {
			Type  string         `json:"type"`
			Usage provider.Usage `json:"usage"`
		}{"usage", e.Usage}, true
	case core.EvTurnEnd:
		return struct {
			Type  string `json:"type"`
			Stop  string `json:"stop"`
			Error string `json:"error,omitempty"`
		}{"turn_end", string(e.Stop), errString(e.Err)}, true
	}
	return nil, false
}

// continuousCompaction derives the automatic compaction policy from the
// resolved model's catalog entry. Unknown context windows disable it.
func continuousCompaction(r Resolved) continuous.CompactionPolicy {
	m, err := provider.FindModel(r.Provider, r.Model)
	if err != nil || m.ContextWindow <= 0 {
		return continuous.CompactionPolicy{}
	}
	reserve := m.MaxOutput
	if reserve <= 0 || reserve > m.ContextWindow/4 {
		reserve = m.ContextWindow / 4
	}
	// Summarize in the background once the context passes roughly half of
	// the usable window, so the blocking threshold is rarely reached.
	return continuous.CompactionPolicy{ContextWindow: m.ContextWindow, ReserveTokens: reserve, KeepRecentTokens: m.ContextWindow / 8, BackgroundTokens: (m.ContextWindow - reserve) / 2}
}
