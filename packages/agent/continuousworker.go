package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
)

// continuousWorkerOptions are the flags of `zot continuous worker`.
type continuousWorkerOptions struct {
	roots       map[string]string // repository name -> directory
	ledger      string
	environment string
	token       string
}

// workerArgs is the call payload of a repository-scoped worker tool: the
// host names the repository, the worker resolves it to a local directory.
type workerArgs struct {
	Repo string          `json:"repo"`
	Args json.RawMessage `json:"args"`
}

func parseContinuousWorkerArgs(args []string) (continuousWorkerOptions, error) {
	opts := continuousWorkerOptions{roots: map[string]string{}, environment: "desktop"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "--root":
			v, err := value()
			if err != nil {
				return opts, err
			}
			name, dir, ok := strings.Cut(v, "=")
			if !ok {
				dir, name = v, filepath.Base(v)
			}
			abs, err := filepath.Abs(dir)
			if err != nil {
				return opts, err
			}
			if info, err := os.Stat(abs); err != nil || !info.IsDir() {
				return opts, fmt.Errorf("root %s is not a directory", dir)
			}
			opts.roots[name] = abs
			opts.roots[abs] = abs
		case "--ledger":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.ledger = v
		case "--environment":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.environment = v
		case "--token":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.token = v
		default:
			return opts, fmt.Errorf("unknown worker flag %s", arg)
		}
	}
	if len(opts.roots) == 0 {
		return opts, errors.New("at least one --root is required")
	}
	return opts, nil
}

// runContinuousWorker serves the continuous worker protocol on stdin and
// stdout. A host (or a process that relays the host connection, such as a
// desktop app) writes requests to stdin and reads responses from stdout.
// Every tool call names a repository; the worker runs the tool jailed to
// that repository's directory and refuses unknown repositories.
func runContinuousWorker(ctx context.Context, args []string, in io.ReadCloser, out io.WriteCloser) error {
	opts, err := parseContinuousWorkerArgs(args)
	if err != nil {
		return err
	}
	reg := core.Registry{}
	for _, name := range []string{"read", "write", "edit", "glob", "bash"} {
		reg[name] = &repoWorkerTool{name: name, roots: opts.roots}
	}
	w := &continuous.WorkerServer{Environment: opts.environment, Tools: reg, Token: opts.token, LedgerPath: opts.ledger}
	if err := w.Open(); err != nil {
		return err
	}
	defer w.Close()
	w.ServeConn(ctx, &stdioConn{ReadCloser: in, WriteCloser: out})
	return nil
}

// stdioConn owns the worker's streams. Closing both endpoints unblocks
// pending reads and writes when ServeConn observes cancellation.
type stdioConn struct {
	io.ReadCloser
	io.WriteCloser
	once sync.Once
	err  error
}

func (c *stdioConn) Close() error {
	c.once.Do(func() {
		c.err = errors.Join(c.ReadCloser.Close(), c.WriteCloser.Close())
	})
	return c.err
}

// repoWorkerTool resolves the call's repository and runs the standard
// tool jailed to it.
type repoWorkerTool struct {
	name  string
	roots map[string]string
}

func (t *repoWorkerTool) Name() string { return t.name }
func (t *repoWorkerTool) Description() string {
	return t.tool("").Description()
}
func (t *repoWorkerTool) Schema() json.RawMessage { return t.tool("").Schema() }

func (t *repoWorkerTool) tool(dir string) core.Tool {
	sb := tools.NewSandbox(dir)
	if dir != "" {
		sb.Lock()
	}
	switch t.name {
	case "read":
		return &tools.ReadTool{CWD: dir, Sandbox: sb}
	case "write":
		return &tools.WriteTool{CWD: dir, Sandbox: sb}
	case "edit":
		return &tools.EditTool{CWD: dir, Sandbox: sb}
	case "glob":
		return &tools.GlobTool{CWD: dir, Sandbox: sb}
	default:
		return &tools.BashTool{CWD: dir, Sandbox: sb}
	}
}

func (t *repoWorkerTool) Execute(ctx context.Context, raw json.RawMessage, progress func(string)) (core.ToolResult, error) {
	var a workerArgs
	if err := json.Unmarshal(raw, &a); err != nil || len(a.Args) == 0 {
		return core.ToolResult{}, fmt.Errorf("worker call needs {repo, args}")
	}
	dir, ok := t.roots[a.Repo]
	if !ok {
		names := make([]string, 0, len(t.roots))
		for k := range t.roots {
			if !filepath.IsAbs(k) {
				names = append(names, k)
			}
		}
		return core.ToolResult{}, fmt.Errorf("repository %q is not shared by this worker (shared: %s)", a.Repo, strings.Join(names, ", "))
	}
	return t.tool(dir).Execute(ctx, a.Args, progress)
}
