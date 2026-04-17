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
	fmt.Fprintf(os.Stderr, `zot %s — lightweight terminal coding agent

usage:
  zot                          interactive tui
  zot "prompt"                 interactive, pre-filled prompt
  zot -p "prompt"              print final text, exit
  zot --json "prompt"          newline-delimited json events, exit

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

  --max-steps N                agent loop iteration cap (default 50)
  --list-models                print known models and exit
  -h, --help                   this message
  -v, --version                version info
`, version)
}
