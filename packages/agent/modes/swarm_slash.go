package modes

import (
	"context"
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/packages/agent/swarm"
	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/tui"
)

// runSwarm dispatches /swarm subcommands. Layout:
//
//	/swarm                       -> open the dashboard
//	/swarm list                  -> open the dashboard
//	/swarm new [--model M] [--provider P] <task...>
//	                             -> spawn an agent (optionally pinned to a model)
//	/swarm kill <id>             -> stop a running agent
//	/swarm remove <id>           -> tear down a terminated agent
//	/swarm logs <id>             -> open the scrollable transcript view
//	/swarm send <id> <text...>   -> send a follow-up user turn to <id>
//	/swarm resume [id]           -> resume an agent (omit id to pick from a list)
//	/swarm attach <id>           -> (planned) drop into the agent's TUI
//
// When cfg.Swarm is nil the command tells the user the feature is
// disabled instead of pretending to work.
func (i *Interactive) runSwarm(ctx context.Context, args []string) {
	if i.cfg.Swarm == nil {
		i.mu.Lock()
		i.statusErr = "swarm is disabled in this build"
		i.statusOK = ""
		i.mu.Unlock()
		return
	}

	sub := ""
	rest := ""
	if len(args) > 0 {
		sub = strings.ToLower(args[0])
		// Guard the args[1:] reslice: when only the subcommand was
		// typed (e.g. bare "/swarm new"), args has length 1 and the
		// naive args[1:] is fine, but when args is empty (bare
		// "/swarm") the reslice is [1:0] and panics. The len>0 branch
		// here keeps both cases safe.
		if len(args) > 1 {
			rest = strings.TrimSpace(strings.Join(args[1:], " "))
		}
	}

	// spawnAdapter, sendAdapter, resumeAdapter wrap the Swarm methods
	// in the signatures the dialog expects. Defined once here so the
	// three Open()-shaped entry points (list, logs/view-jump, resume)
	// feed the dialog identical callbacks.
	spawnAdapter := func(task, model, provider string) error {
		_, err := i.cfg.Swarm.SpawnReq(ctx, swarm.SpawnRequest{
			Task: task, Model: model, Provider: provider,
		})
		return err
	}
	sendAdapter := func(id, text string) error {
		return i.cfg.Swarm.SendUserTurn(id, text)
	}
	resumeAdapter := func(id string) error {
		_, err := i.cfg.Swarm.Resume(ctx, id)
		return err
	}

	// Pin every fresh spawn to whatever the host's /model selection
	// is right now. This is captured at /swarm time — if the user
	// wants a different model for the next swarm agent, they pick it
	// via /model first (globally), or, while inside the spawn
	// editor, by typing /model on its own line to pop the picker.
	i.swarmDialog.spawnWithImages = func(task, model, provider string, images []provider.ImageBlock) error {
		_, err := i.cfg.Swarm.SpawnReq(ctx, swarm.SpawnRequest{Task: task, Model: model, Provider: provider, Images: images})
		return err
	}
	i.swarmDialog.sendWithImages = i.cfg.Swarm.SendUserTurnWithImages
	i.swarmDialog.fileContext = ctx
	i.swarmDialog.sandbox = i.cfg.Sandbox
	i.swarmDialog.transferFiles = i.hasPromptDriver()
	i.swarmDialog.SetCompactMode(i.compactModeEnabled())
	i.swarmDialog.SetLineInput(tui.NormalizeInputStyle(i.cfg.TUIInputStyle) == tui.InputStyleLines)
	i.swarmDialog.SetCurrentModel(i.cfg.Model, i.cfg.Provider)
	if i.cfg.LoggedInProviders != nil {
		i.swarmDialog.SetLoggedInProviders(i.cfg.LoggedInProviders())
	}

	switch sub {
	case "", "list", "ls", "ps":
		i.swarmDialog.Open(
			i.cfg.Swarm.SnapshotAll,
			i.cfg.Swarm.Stop,
			i.cfg.Swarm.Remove,
			spawnAdapter,
			sendAdapter,
			resumeAdapter,
			i.cfg.CWD,
		)
	case "new", "spawn":
		if rest == "" {
			i.swarmStatus("", "/swarm new <task>: missing task")
			return
		}
		// Permit `--model X --provider Y` flags before the task so
		// scripts can pin a model without going through the dialog.
		// Anything that isn't a recognised flag terminates parsing
		// and the rest becomes the task; this keeps `/swarm new
		// --model foo do a thing` and `/swarm new do --model thing`
		// (where --model is part of the task) unambiguous — only
		// leading flags are consumed.
		model, provider, task := parseSpawnFlags(rest)
		if task == "" {
			i.swarmStatus("", "/swarm new: missing task (after any --model/--provider flags)")
			return
		}
		a, err := i.cfg.Swarm.SpawnReq(ctx, swarm.SpawnRequest{
			Task: task, Model: model, Provider: provider,
		})
		if err != nil {
			i.swarmStatus("", "spawn: "+err.Error())
			return
		}
		if model != "" {
			i.swarmStatus("spawned "+a.ID+" (model "+model+")", "")
		} else {
			i.swarmStatus("spawned "+a.ID, "")
		}
	case "kill", "stop":
		if rest == "" {
			i.swarmStatus("", "/swarm kill <id>: missing id")
			return
		}
		if err := i.cfg.Swarm.Stop(rest); err != nil {
			i.swarmStatus("", "kill: "+err.Error())
			return
		}
		i.swarmStatus("stopped "+rest, "")
	case "remove", "rm":
		if rest == "" {
			i.swarmStatus("", "/swarm remove <id>: missing id")
			return
		}
		if err := i.cfg.Swarm.Remove(rest); err != nil {
			i.swarmStatus("", "remove: "+err.Error())
			return
		}
		i.swarmStatus("removed "+rest, "")
	case "logs", "log", "view":
		if rest == "" {
			i.swarmStatus("", "/swarm logs <id>: missing id")
			return
		}
		ok := i.swarmDialog.OpenViewing(
			rest,
			i.cfg.Swarm.SnapshotAll,
			i.cfg.Swarm.Stop,
			i.cfg.Swarm.Remove,
			spawnAdapter,
			sendAdapter,
			resumeAdapter,
			i.cfg.CWD,
		)
		if !ok {
			i.swarmStatus("", "/swarm logs: no agent matching "+rest)
		}
	case "resume", "reattach", "reopen":
		if rest == "" {
			// No id given: open the dashboard with the cursor
			// pre-positioned on the first resumable agent, and
			// tell the user how many there are so they know what
			// to expect. Pressing R confirms; ↑/↓ to pick a
			// different row first.
			count := i.swarmDialog.OpenForResume(
				i.cfg.Swarm.SnapshotAll,
				i.cfg.Swarm.Stop,
				i.cfg.Swarm.Remove,
				spawnAdapter,
				sendAdapter,
				resumeAdapter,
				i.cfg.CWD,
			)
			switch count {
			case 0:
				i.swarmStatus("", "/swarm resume: no resumable agents (none detached or terminated)")
			case 1:
				i.swarmStatus("1 resumable agent, press R to resume", "")
			default:
				i.swarmStatus(fmt.Sprintf("%d resumable agents, ↑/↓ to pick, R to resume", count), "")
			}
			return
		}
		a, err := i.cfg.Swarm.Resume(ctx, rest)
		if err != nil {
			i.swarmStatus("", "resume: "+err.Error())
			return
		}
		i.swarmStatus("resumed "+a.ID, "")
	case "send", "prompt", "msg":
		// /swarm send <id> <text...> is the non-interactive
		// counterpart of pressing 'p' in the dashboard. We split the
		// joined `rest` ourselves rather than reusing the dispatcher's
		// already-fielded args[] because the text may contain spaces
		// the user expects to be preserved verbatim.
		id, text := splitIDAndRest(rest)
		if id == "" {
			i.swarmStatus("", "/swarm send <id> <text>: missing id")
			return
		}
		if text == "" {
			i.swarmStatus("", "/swarm send <id> <text>: missing text")
			return
		}
		if err := i.cfg.Swarm.SendUserTurn(id, text); err != nil {
			i.swarmStatus("", friendlySendErr(id, err))
			return
		}
		i.swarmStatus("sent to "+id, "")
	case "attach":
		// PTY-reparenting is a significant chunk of work I haven't
		// landed yet (see the design sketch). Recognise the name so
		// /swarm attach doesn't fall through to the generic "unknown
		// subcommand" path — that error message is misleading because
		// it makes attach sound like a typo instead of a planned
		// feature. Point the user at /swarm logs in the meantime.
		i.swarmStatus("", "/swarm attach: not implemented yet (needs PTY reparenting). Use /swarm logs "+firstWord(rest)+" to watch its transcript.")
	default:
		i.swarmStatus("", "/swarm: unknown subcommand "+sub+" (try list / new / kill / remove / logs / send / resume)")
	}
}

// parseSpawnFlags consumes any leading `--model X` / `--provider Y`
// flags from s and returns them along with the remaining task body.
// We deliberately only honour LEADING flags so a task like "check
// --model lookup" doesn't accidentally swallow part of its prose as
// the model name.
//
// Recognised forms:
//
//	--model X            two-token form
//	--model=X            single-token form
//	--provider X         two-token form
//	--provider=X         single-token form
func parseSpawnFlags(s string) (model, provider, task string) {
	fields := strings.Fields(s)
	i := 0
	for i < len(fields) {
		f := fields[i]
		switch {
		case f == "--model":
			// Consume the flag even when no value follows so a
			// dangling "--model" doesn't leak into the task. The
			// caller surfaces "missing task" instead.
			if i+1 < len(fields) {
				model = fields[i+1]
				i += 2
			} else {
				i++
			}
			continue
		case strings.HasPrefix(f, "--model="):
			model = strings.TrimPrefix(f, "--model=")
			i++
			continue
		case f == "--provider":
			if i+1 < len(fields) {
				provider = fields[i+1]
				i += 2
			} else {
				i++
			}
			continue
		case strings.HasPrefix(f, "--provider="):
			provider = strings.TrimPrefix(f, "--provider=")
			i++
			continue
		}
		break
	}
	task = strings.TrimSpace(strings.Join(fields[i:], " "))
	return
}

// splitIDAndRest splits "<id> <text...>" into (id, text). The text
// half preserves all whitespace after the first token so the agent
// receives the user's prompt verbatim (modulo a single trim of the
// boundary space). Returns ("", "") when s is empty so the caller
// can surface a missing-id error.
func splitIDAndRest(s string) (id, text string) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", ""
	}
	cut := strings.IndexAny(s, " \t")
	if cut < 0 {
		return s, ""
	}
	return s[:cut], strings.TrimLeft(s[cut+1:], " \t")
}

// firstWord returns the first whitespace-separated token of s, or
// "<id>" when s is empty. Used to keep the "/swarm attach" hint
// readable even when the user typed no argument.
func firstWord(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "<id>"
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i]
	}
	return s
}

func (i *Interactive) swarmStatus(ok, errMsg string) {
	i.mu.Lock()
	i.statusOK = ok
	i.statusErr = errMsg
	i.mu.Unlock()
	i.invalidate()
}
