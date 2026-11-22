package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/patriceckhart/zot/packages/provider"
	"github.com/patriceckhart/zot/packages/provider/auth"
	"golang.org/x/term"
)

const authCommandHelp = `usage:
  zot auth set <provider>           read an API key from a hidden terminal prompt
  zot auth set <provider> --stdin   read an API key from redirected stdin
  zot auth --help

Validates the key where supported and saves it to $ZOT_HOME/auth.json.
Custom providers must be configured in $ZOT_HOME/models.json first.
Keys are not accepted as command-line arguments. Use stdin for scripts.
OAuth login remains available through /login in the interactive TUI.
`

type authSetOptions struct {
	provider string
	stdin    bool
}

func parseAuthSetOptions(args []string) (authSetOptions, error) {
	var opts authSetOptions
	if len(args) < 2 || args[0] != "set" || strings.HasPrefix(args[1], "-") || strings.TrimSpace(args[1]) == "" {
		return opts, errors.New("expected auth set <provider> [--stdin]")
	}
	opts.provider = args[1]
	if len(args) == 3 && args[2] == "--stdin" {
		opts.stdin = true
	} else if len(args) != 2 {
		// Do not echo arguments, which may contain an accidentally pasted key.
		return opts, errors.New("keys are not accepted as arguments, use auth set <provider> --stdin or the hidden prompt")
	}
	return opts, nil
}

// runAuthCommand runs before agent startup and never loads sessions or tools.
func runAuthCommand(rawArgs []string) (bool, error) {
	if len(rawArgs) == 0 || rawArgs[0] != "auth" {
		return false, nil
	}
	args := rawArgs[1:]
	if len(args) == 0 || (len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h")) {
		_, err := io.WriteString(os.Stdout, authCommandHelp)
		return true, err
	}
	opts, err := parseAuthSetOptions(args)
	if err != nil {
		return true, err
	}
	// Only load configured providers, without discovery, config repairs, or
	// other runtime startup work. This command must also work before login.
	LoadUserModels()
	registerCustomAPIKeyProviders()
	if !auth.SupportsAPIKeyLogin(opts.provider) {
		return true, errors.New("unknown API-key provider, configure custom providers in models.json first")
	}
	key, err := readAuthKey(os.Stdin, os.Stderr, opts.stdin)
	if err != nil {
		return true, err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	manager := auth.NewManager(AuthStoreFor())
	defer manager.Close()
	if err := manager.LoginAPIKey(ctx, opts.provider, key); err != nil {
		return true, err
	}
	_, err = fmt.Fprintf(os.Stdout, "saved API key for %s\n", opts.provider)
	return true, err
}

func registerCustomAPIKeyProviders() {
	var names []string
	for name := range provider.CustomProviders() {
		if !isBuiltinProvider(name) {
			names = append(names, name)
		}
	}
	auth.SetExtraAPIKeyProviders(names)
}

var (
	authIsTerminal   = term.IsTerminal
	authReadPassword = readAuthTerminalKey
)

func readAuthKey(in *os.File, prompt io.Writer, fromStdin bool) (string, error) {
	isTerminal := authIsTerminal(int(in.Fd()))
	if fromStdin {
		if isTerminal {
			return "", errors.New("--stdin requires redirected input, omit it for a hidden terminal prompt")
		}
		return readAuthKeyStdin(in)
	}
	if !isTerminal {
		return "", errors.New("no terminal, use auth set <provider> --stdin with redirected input")
	}
	value, err := authReadPassword(in, prompt)
	if err != nil {
		return "", errors.New("could not read API key from terminal")
	}
	return string(value), nil
}

// Raw mode lets x/term handle Ctrl+C as input, so deferred restoration runs
// instead of the process exiting with terminal echo still disabled.
func readAuthTerminalKey(in *os.File, out io.Writer) ([]byte, error) {
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	defer term.Restore(int(in.Fd()), state)
	return readAuthTerminalPassword(in, out)
}

func readAuthTerminalPassword(in io.Reader, out io.Writer) ([]byte, error) {
	terminal := term.NewTerminal(struct {
		io.Reader
		io.Writer
	}{in, out}, "")
	value, err := terminal.ReadPassword("API key (hidden): ")
	return []byte(value), err
}

func readAuthKeyStdin(in io.Reader) (string, error) {
	const maxBytes = 64 * 1024
	value, err := io.ReadAll(io.LimitReader(in, maxBytes+1))
	if err != nil {
		return "", errors.New("could not read API key from stdin")
	}
	if len(value) > maxBytes {
		return "", errors.New("API key input exceeds 64 KiB")
	}
	return string(value), nil
}
