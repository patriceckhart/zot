package agent

import (
	"fmt"
	"os"
	"strings"
)

// Mode is the CLI run mode.
type Mode string

const (
	ModeInteractive Mode = "interactive"
	ModePrint       Mode = "print"
	ModeJSON        Mode = "json"
	ModeRPC         Mode = "rpc"
)

// Args holds parsed command-line options.
type Args struct {
	Mode     Mode
	Provider string
	Model    string
	APIKey   string

	BaseURL            string // override provider base URL (for tests/self-hosted)
	SystemPrompt       string
	AppendSystemPrompt []string
	Reasoning          string

	Continue bool
	Resume   bool
	Session  string
	NoSess   bool

	CWD      string
	NoTools  bool
	Tools    []string
	MaxSteps int

	// Exts is a list of directory paths the user passed via --ext.
	// Each must contain an extension.json. Loaded for one session
	// only; never persisted. Take precedence over installed exts of
	// the same name.
	Exts []string

	// NoExt disables extension discovery + spawn entirely for this
	// run. --ext PATH still works (explicit beats implicit) so you
	// can run "with only this one extension" via --no-ext --ext PATH.
	NoExt bool

	// NoSkill disables ALL skill discovery for this run, including
	// the built-in skills compiled into the binary. The system
	// prompt loses its "Available skills" manifest and the `skill`
	// tool isn't registered. Useful for running zot without any
	// extra context biasing the model.
	NoSkill bool

	// WithSkills opts into loading user-installed skills from
	// $ZOT_HOME/skills/, .zot/skills/, .claude/skills/, and
	// .agents/skills/. Without this flag only the built-in skills
	// shipped with the zot binary are available, so a fresh install
	// has a deterministic skill set regardless of what's lying
	// around in the user's home directory.
	WithSkills bool

	// NoYolo turns on per-tool confirmation. Before each tool
	// invocation the TUI prompts the user with the tool name + args
	// and waits for an explicit yes/no. The user can also pick
	// "always for this tool this session" or "always for anything
	// this session" to stop being prompted again. Defaults off
	// (yolo mode): tools run without asking.
	//
	// No effect in -p / --json / rpc modes, which have no
	// interactive prompt. A warning is printed to stderr on startup
	// so scripts know the flag is ignored, but tools still run
	// freely so automated workflows keep working.
	NoYolo bool

	ListModels bool
	Help       bool
	Version    bool

	Prompt string // concatenated positional args
}

// ParseArgs parses the process arguments (excluding argv[0]).
func ParseArgs(in []string) (Args, error) {
	a := Args{Mode: ModeInteractive, MaxSteps: 50}
	positional := []string{}

	want := func(i *int, flag string) (string, error) {
		*i++
		if *i >= len(in) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return in[*i], nil
	}

	for i := 0; i < len(in); i++ {
		arg := in[i]
		switch arg {
		case "-h", "--help":
			a.Help = true
		case "-v", "--version":
			a.Version = true
		case "-p", "--print":
			a.Mode = ModePrint
		case "--json":
			a.Mode = ModeJSON
		case "--rpc":
			a.Mode = ModeRPC
		case "-c", "--continue":
			a.Continue = true
		case "-r", "--resume":
			a.Resume = true
		case "--no-session":
			a.NoSess = true
		case "--no-tools":
			a.NoTools = true
		case "--list-models":
			a.ListModels = true
		case "--experimental-oauth":
			// deprecated: subscription login is always available.
			// accepted silently for backwards compatibility.
		case "--provider":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.Provider = v
		case "--model":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.Model = v
		case "--api-key":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.APIKey = v
		case "--base-url":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.BaseURL = v
		case "--system-prompt":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.SystemPrompt = v
		case "--append-system-prompt":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.AppendSystemPrompt = append(a.AppendSystemPrompt, v)
		case "--ext", "-e":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			// Repeatable; each value is a directory containing an
			// extension.json. Resolved to absolute later so paths like
			// "." survive a later cwd change.
			a.Exts = append(a.Exts, v)
		case "--no-ext", "--no-extensions":
			a.NoExt = true
		case "--no-skill", "--no-skills":
			a.NoSkill = true
		case "--with-skills", "--with-skill":
			a.WithSkills = true
		case "--no-yolo":
			a.NoYolo = true
		case "--reasoning":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			switch strings.ToLower(v) {
			case "", "low", "medium", "high":
				a.Reasoning = strings.ToLower(v)
			default:
				return a, fmt.Errorf("--reasoning must be low|medium|high")
			}
		case "--session":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.Session = v
		case "--cwd":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			a.CWD = v
		case "--tools":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			for _, t := range strings.Split(v, ",") {
				t = strings.TrimSpace(t)
				if t != "" {
					a.Tools = append(a.Tools, t)
				}
			}
		case "--max-steps":
			v, err := want(&i, arg)
			if err != nil {
				return a, err
			}
			var n int
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n <= 0 {
				return a, fmt.Errorf("--max-steps must be a positive integer")
			}
			a.MaxSteps = n
		default:
			if strings.HasPrefix(arg, "-") && arg != "-" {
				return a, fmt.Errorf("unknown flag %q", arg)
			}
			positional = append(positional, arg)
		}
	}

	if len(positional) > 0 {
		a.Prompt = strings.Join(positional, " ")
	}

	if a.CWD == "" {
		a.CWD, _ = os.Getwd()
	}
	return a, nil
}

// PrintHelp writes the help text to w (stderr by default).
func PrintHelp(version string) {
	fmt.Fprintf(os.Stderr, `zot %s — Yet another coding agent harness.

usage:
  zot                          interactive tui
  zot "prompt"                 interactive, pre-filled prompt
  zot -p "prompt"              print final text, exit
  zot --json "prompt"          newline-delimited json events, exit
  zot rpc                      json-rpc loop on stdin/stdout (see docs/rpc.md)
  zot ext help                 extension manager help
  zot ext list                 list installed extensions
  zot ext install <path|url>   install an extension into $ZOT_HOME/extensions/
  zot telegram-bot setup       configure a telegram bot (from BotFather)
  zot telegram-bot run         foreground bridge (ctrl+c to stop)
  zot telegram-bot start       background bridge (detached)
  zot telegram-bot stop        stop the background bridge
  zot telegram-bot logs [-f]   tail the background bridge's log
  zot telegram-bot status      config + running state
  zot telegram-bot reset       forget saved token
  (short alias: zot tg ...)

flags:
  --provider  anthropic|openai
  --model     model id (see --list-models)
  --api-key   api key for this run (falls back to env and auth.json)
  --base-url  override provider api base url (for testing/self-hosted)

  --system-prompt TEXT         replace the default system prompt
  --append-system-prompt TEXT  append to the system prompt (repeatable)
  --reasoning low|medium|high  enable reasoning on supported models

  -c, --continue               continue the most recent session for this cwd
  -r, --resume                 pick a session to resume
  --session PATH               resume a specific session file
  --no-session                 do not read or write a session file

  --cwd PATH                   treat PATH as the working directory
  --no-tools                   disable all tools
  --tools csv                  only enable the listed tools
  -e, --ext PATH               load an extension from PATH for this run
                               (repeatable; takes precedence over
                               installed extensions of the same name)
  --no-ext                     skip extension discovery for this run
                               (--ext PATH still works on top, so
                               --no-ext --ext ./x runs only x)
  --no-skill                   skip skill discovery for this run,
                               including built-in skills (no skill
                               tool, no Available skills manifest)
  --with-skills                load user-installed skills from
                               $ZOT_HOME/skills/ + .zot/skills/ +
                               .claude/skills/ + .agents/skills/.
                               default: only built-in skills load

  --no-yolo                    ask before running every tool call
                               (interactive tui only; ignored with
                               a stderr warning in -p / --json / rpc)

  --max-steps N                agent loop iteration cap (default 50)
  --list-models                print known models and exit
  -h, --help                   this message
  -v, --version                version info

extensions:
  zot ext install ./path/to/ext    install a local extension
  zot ext install https://...git   clone + install from git
  zot --ext ./path/to/ext          load an extension for this run only
  zot ext help                     show all extension subcommands
`, version)
}
