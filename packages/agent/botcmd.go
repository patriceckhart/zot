package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/patriceckhart/zot/packages/agent/modes/bot"
	"github.com/patriceckhart/zot/packages/agent/modes/telegram"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// detachChild configures cmd to run in its own process group so tty
// signals sent to the parent (SIGINT, SIGHUP on logout) don't also
// reach the detached bot. Platform-specific: setsid on unix, a noop
// on windows (Go's spawn path already detaches when no console is
// inherited). See botcmd_unix.go and botcmd_windows.go.
var detachChild func(cmd *exec.Cmd)

// runBotCommand dispatches `zot <protocol>-bot ...` subcommands via
// the botSpec registry. Returns true if rawArgs begins with a
// recognised subcommand, false otherwise. The short alias "tg" (and
// "mx" for matrix) are also accepted.
func runBotCommand(rawArgs []string, version string) (handled bool, err error) {
	if len(rawArgs) == 0 {
		return false, nil
	}
	spec := specFor(rawArgs[0])
	if spec == nil {
		return false, nil
	}
	sub := ""
	var tail []string
	if len(rawArgs) >= 2 {
		sub = rawArgs[1]
		tail = rawArgs[2:]
	}
	switch sub {
	case "", "help", "-h", "--help":
		spec.printHelp()
		return true, nil
	case "setup":
		return true, spec.setup(tail)
	case "status":
		return true, spec.status()
	case "reset":
		return true, spec.reset()
	case "run":
		return true, botRun(spec, tail, version)
	case "start":
		return true, botStart(spec, tail)
	case "stop":
		return true, botStop(spec)
	case "logs":
		return true, botLogs(spec, tail)
	default:
		spec.printHelp()
		return true, fmt.Errorf("unknown %s subcommand %q", spec.subcommand, sub)
	}
}

// botStart launches `zot <spec.subcommand> run` as a detached child
// process, writes its pid to the spec's pid file, and returns
// immediately. Stdout/stderr of the child are redirected to the spec's
// log file.
func botStart(spec *botSpec, rawTail []string) error {
	// Refuse to start if another bot is already running.
	if pid, alive, _ := bot.IsRunningAt(spec.pidPath(ZotHome())); alive {
		return fmt.Errorf("%s is already running (pid %d); use `zot %s stop` first", spec.name, pid, spec.subcommand)
	}
	_ = bot.RemovePIDFile(spec.pidPath(ZotHome())) // clear any stale pid file

	if ok, cerr := spec.configured(ZotHome()); cerr != nil {
		return cerr
	} else if !ok {
		return fmt.Errorf("%s is not configured — run `zot %s setup` first", spec.name, spec.subcommand)
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate zot binary: %w", err)
	}

	logPath := spec.logPath(ZotHome())
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	defer logFile.Close()

	// Refuse to start from a `go run` temp binary: Go deletes the
	// binary when `go run` exits, which breaks the detached child.
	// Users hit cryptic tls / exec errors on that path; fail clearly.
	if strings.Contains(self, string(os.PathSeparator)+"go-build") ||
		strings.Contains(self, string(os.PathSeparator)+"go-tmp") {
		return fmt.Errorf("detected `go run` temp binary at %s — run `make install` (or copy ./bin/zot to your PATH) and use the installed binary for `start`", self)
	}

	// Child argv: same flags the user passed to `zot <sub> start`,
	// mapped to `zot <sub> run`. Preserves --provider, --model, --cwd, etc.
	args := append([]string{spec.subcommand, "run"}, rawTail...)
	cmd := exec.Command(self, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	// Detach: new session / new process group so terminal signals
	// don't reach the child. Impl lives in botcmd_unix.go /
	// botcmd_windows.go because Setsid is posix-only.
	detachChild(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn: %w", err)
	}
	if err := bot.WritePIDFile(spec.pidPath(ZotHome()), cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		return fmt.Errorf("write pid: %w", err)
	}
	// Don't wait() — detach. OS will reparent the child to init when we exit.
	go func() { _ = cmd.Process.Release() }()

	fmt.Printf("started zot %s as pid %d (logs: %s)\n", spec.subcommand, cmd.Process.Pid, logPath)
	fmt.Printf("use `zot %s logs -f` to tail, `zot %s stop` to stop.\n", spec.subcommand, spec.subcommand)
	return nil
}

// botStop sends SIGTERM to the running bot (SIGKILL if it doesn't
// exit within 5s) and cleans up the pid file.
func botStop(spec *botSpec) error {
	pid, alive, err := bot.IsRunningAt(spec.pidPath(ZotHome()))
	if err != nil {
		return err
	}
	if !alive {
		if pid > 0 {
			_ = bot.RemovePIDFile(spec.pidPath(ZotHome()))
			fmt.Printf("no live process; cleared stale pid %d\n", pid)
			return nil
		}
		fmt.Printf("%s is not running\n", spec.name)
		return nil
	}
	if err := bot.StopProcess(pid, 5*time.Second); err != nil {
		return fmt.Errorf("stop pid %d: %w", pid, err)
	}
	_ = bot.RemovePIDFile(spec.pidPath(ZotHome()))
	fmt.Printf("stopped pid %d\n", pid)
	return nil
}

// botLogs prints (or tails with --follow) the bot log file.
func botLogs(spec *botSpec, rawTail []string) error {
	follow := false
	for _, a := range rawTail {
		if a == "-f" || a == "--follow" {
			follow = true
		}
	}
	p := spec.logPath(ZotHome())
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("no log file at", p)
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(os.Stdout, f); err != nil {
		return err
	}
	if !follow {
		return nil
	}

	// Naive tail -f: poll for new bytes until ctrl+c.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigc)
	for {
		select {
		case <-sigc:
			return nil
		case <-time.After(500 * time.Millisecond):
			if _, err := io.Copy(os.Stdout, f); err != nil {
				return err
			}
		}
	}
}

// botRun starts the polling loop in the foreground. Ctrl+C stops it.
func botRun(spec *botSpec, rawTail []string, version string) error {
	// Parse only a small subset of flags relevant to bot run. We reuse
	// the main args parser so --provider/--model/--cwd/--api-key/--reasoning
	// behave the same as in the tui.
	args, err := ParseArgs(rawTail)
	if err != nil {
		return err
	}

	// Bot mode always requires credentials (can't pop a /login dialog).
	resolved, err := Resolve(args, true)
	if err != nil {
		return err
	}

	if ok, cerr := spec.configured(ZotHome()); cerr != nil {
		return cerr
	} else if !ok {
		return fmt.Errorf("%s is not configured — run `zot %s setup` first", spec.name, spec.subcommand)
	}
	adapter, err := spec.newAdapter(ZotHome())
	if err != nil {
		return err
	}

	agent := resolved.NewAgent()

	// Session: optional, same model as the tui. Persist so DMs build on
	// prior context. --no-session disables.
	var sess *core.Session
	if !args.NoSess {
		s, _, serr := openOrCreateSessionForBot(args, resolved, agent, version)
		if serr == nil {
			sess = s
			agent.OnMessageAppended = func(msg provider.Message) {
				_ = sess.AppendMessage(msg)
			}
			agent.OnUsage = func(u provider.Usage) {
				_ = sess.AppendUsage(u, u)
			}
			agent.OnTranscriptCompacted = func(msgs []provider.Message) {
				_ = sess.AppendCompaction(msgs)
			}
			defer sess.Close()
		} else {
			fmt.Fprintln(os.Stderr, "session:", serr)
		}
	}

	var runner *bot.Runner
	runner = bot.NewRunner(adapter, agent, bot.Config{
		ZotHome:    ZotHome(),
		Provider:   resolved.Provider,
		Model:      resolved.Model,
		AuthMethod: resolved.AuthMethod,
		CWD:        args.CWD,
		RefreshCreds: func() error {
			// Re-run the same resolver the tui uses so we pick up
			// refreshed oauth tokens, re-logins, and model switches.
			// Only the provider client is swapped — tools, sandbox,
			// system prompt, and transcript stay with the existing agent.
			next, err := Resolve(args, true)
			if err != nil {
				return err
			}
			agent.Client = next.NewClient()
			agent.Model = next.Model
			runner.UpdateRuntimeConfig(next.Provider, next.Model, next.AuthMethod, next.CWD)
			return nil
		},
	})

	// Record our pid so `zot <sub> status` / `zot <sub> stop` can find us,
	// regardless of whether we were started directly or via `start`.
	_ = bot.WritePIDFile(spec.pidPath(ZotHome()), os.Getpid())
	defer bot.RemovePIDFile(spec.pidPath(ZotHome()))

	// Translate sigterm/sigint into a context cancel so the bot's goroutines
	// and the currently-running turn wind down cleanly.
	ctx, cancel := context.WithCancel(context.Background())
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		cancel()
	}()
	defer cancel()
	return runner.Run(ctx)
}

// openOrCreateSessionForBot reuses the same logic as interactive mode
// but never prompts (no TTY picker); falls back to latest or new.
func openOrCreateSessionForBot(args Args, r Resolved, ag *core.Agent, version string) (*core.Session, []any, error) {
	if args.Continue {
		if latest := core.LatestSession(ZotHome(), args.CWD); latest != "" {
			s, msgs, err := core.OpenSession(latest)
			if err != nil {
				return nil, nil, err
			}
			ag.SetMessages(msgs)
			return s, nil, nil
		}
	}
	s, err := core.NewSession(ZotHome(), args.CWD, r.Provider, r.Model, version)
	return s, nil, err
}

// maskToken returns "123456:ABC...xyz" so copies of zot telegram-bot status can be
// pasted into bug reports without leaking the full token.
func maskToken(tok string) string {
	if len(tok) <= 10 {
		return "<hidden>"
	}
	// telegram tokens look like "123456789:ABCD..." — keep the id, mask the body.
	i := strings.IndexByte(tok, ':')
	if i < 0 {
		return tok[:4] + "..." + tok[len(tok)-4:]
	}
	body := tok[i+1:]
	if len(body) < 8 {
		return tok[:i+1] + "<hidden>"
	}
	return tok[:i+1] + body[:3] + "..." + body[len(body)-3:]
}

// telegramSpec wires the existing Telegram setup/status/reset flows
// into the generic dispatcher. Bodies are the pre-refactor botSetup,
// botStatus, botReset, printBotHelp — unchanged.
func telegramSpec() *botSpec {
	return &botSpec{
		name:       "telegram",
		subcommand: "telegram-bot",
		aliases:    []string{"tg"},
		pidPath:    telegram.PIDPath,
		logPath:    telegram.LogPath,
		configured: func(zotHome string) (bool, error) {
			cfg, err := telegram.LoadConfig(zotHome)
			if err != nil {
				return false, err
			}
			return cfg.BotToken != "", nil
		},
		printHelp: printTelegramBotHelp,
		setup:     telegramBotSetup,
		status:    telegramBotStatus,
		reset:     telegramBotReset,
		newAdapter: func(zotHome string) (bot.BotAdapter, error) {
			cfg, err := telegram.LoadConfig(zotHome)
			if err != nil {
				return nil, err
			}
			return telegram.NewAdapter(
				telegram.NewClient(cfg.BotToken),
				&cfg,
				func(c telegram.Config) error { return telegram.SaveConfig(zotHome, c) },
			), nil
		},
	}
}

// printTelegramBotHelp prints usage for `zot telegram-bot`.
func printTelegramBotHelp() {
	fmt.Fprint(os.Stderr, `zot telegram-bot — telegram bridge

usage:
  zot telegram-bot setup                       paste a BotFather token, verify, save
  zot telegram-bot status                      show bridge config and whether it's running
  zot telegram-bot run [flags]                 run in the foreground (ctrl+c to stop)
  zot telegram-bot start [flags]               launch in background, detach, return immediately
  zot telegram-bot stop                        sigterm the running background bot, sigkill if needed
  zot telegram-bot logs [--follow]             tail the background bot's log file
  zot telegram-bot reset                       forget token + allowed user

setup flow:
  1. talk to @BotFather on telegram, /newbot, copy the token
  2. run "zot telegram-bot setup" and paste the token
  3. run "zot telegram-bot start" (background) or "zot telegram-bot run" (foreground)
  4. send /start to your bot from telegram; the first sender claims it

while the bot is running, dm it anything and the message is forwarded
to the agent the same way it would be from the tui. image attachments
(photos or image/* documents) are passed to vision-capable models.
telegram commands the bot handles directly: /help, /status, /stop.

config & state:
  $ZOT_HOME/bot.json       # bot token + paired user (mode 0600)
  $ZOT_HOME/bot.pid        # pid of the running bot (written by run/start)
  $ZOT_HOME/logs/bot.log   # stdout+stderr from "zot telegram-bot start"
`)
}

// telegramBotSetup interactively reads a bot token, verifies it via getMe, and saves it.
func telegramBotSetup(_ []string) error {
	cfg, err := telegram.LoadConfig(ZotHome())
	if err != nil {
		return err
	}

	fmt.Print("telegram bot token (from @BotFather): ")
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return fmt.Errorf("no token provided")
	}

	client := telegram.NewClient(token)
	me, err := client.GetMe(context.Background())
	if err != nil {
		return fmt.Errorf("token rejected by telegram: %w", err)
	}
	cfg.BotToken = token
	cfg.BotUsername = me.Username
	cfg.BotID = me.ID
	// Any stored pairing might be for a different bot; clear it.
	cfg.AllowedUserID = 0
	cfg.LastUpdateID = 0
	if err := telegram.SaveConfig(ZotHome(), cfg); err != nil {
		return err
	}
	fmt.Printf("\nsaved: @%s (id=%d) to %s\n", me.Username, me.ID, telegram.ConfigPath(ZotHome()))
	fmt.Println("next: run `zot telegram-bot run`, then send /start to your bot from telegram.")
	return nil
}

// telegramBotStatus prints the current bot config without the token, plus
// whether the background process is alive.
func telegramBotStatus() error {
	cfg, err := telegram.LoadConfig(ZotHome())
	if err != nil {
		return err
	}
	if cfg.BotToken == "" {
		fmt.Println("telegram: not configured (run `zot telegram-bot setup`)")
		return nil
	}
	maskedTok := maskToken(cfg.BotToken)
	fmt.Printf("telegram bot: @%s (id=%d)\n", cfg.BotUsername, cfg.BotID)
	fmt.Printf("token:        %s\n", maskedTok)
	if cfg.AllowedUserID == 0 {
		fmt.Println("paired with:  (unpaired — send /start from telegram to claim)")
	} else {
		fmt.Printf("paired with:  telegram user id %d\n", cfg.AllowedUserID)
	}
	fmt.Printf("last update:  %d\n", cfg.LastUpdateID)
	fmt.Printf("config file:  %s\n", telegram.ConfigPath(ZotHome()))

	pid, alive, _ := telegram.IsRunning(ZotHome())
	switch {
	case alive:
		fmt.Printf("process:      running (pid %d)\n", pid)
	case pid > 0:
		fmt.Printf("process:      stopped (stale pid %d in %s)\n", pid, telegram.PIDPath(ZotHome()))
	default:
		fmt.Println("process:      stopped")
	}
	logPath := telegram.LogPath(ZotHome())
	if fi, err := os.Stat(logPath); err == nil {
		fmt.Printf("log file:     %s (%d bytes)\n", logPath, fi.Size())
	}
	return nil
}

// telegramBotReset wipes the on-disk bot.json entry.
func telegramBotReset() error {
	p := telegram.ConfigPath(ZotHome())
	if _, err := os.Stat(p); os.IsNotExist(err) {
		fmt.Println("no bot config to reset")
		return nil
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	fmt.Println("removed", p)
	return nil
}
