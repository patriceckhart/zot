package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
)

// attachContinuous connects the interactive TUI to a running host. The
// returned driver replaces the in-process agent loop for prompts, the label is
// shown in the status bar, and close detaches without cancelling host work.
// The agent's transcript is replaced with the host's committed context; a nil
// agent (no credential) is allowed because the host holds the credentials.
func attachContinuous(ctx context.Context, args Args, r Resolved, ag *core.Agent) (func(context.Context, *core.Agent, string, func(core.AgentEvent)) error, string, func(), error) {
	dial, err := continuousDialer(args)
	if err != nil {
		return nil, "", nil, err
	}
	client, err := dial(ctx)
	if err != nil {
		return nil, "", nil, err
	}
	workspace := continuousWorkspace(args, r)
	var conv continuous.Conversation
	if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": workspace, "config": continuous.AgentConfig{Provider: r.Provider, Model: r.Model, Reasoning: r.Reasoning}}, &conv); err != nil {
		client.Close()
		return nil, "", nil, fmt.Errorf("open workspace conversation on host: %w", err)
	}
	driver := &continuous.AttachedDriver{Client: client, ConversationID: conv.ID}
	if ag != nil {
		if _, err := driver.Load(ctx, ag); err != nil {
			client.Close()
			return nil, "", nil, err
		}
	}
	label := "attached: " + shortAddress(args.Continuous)
	return driver.Prompt, label, func() { client.Close() }, nil
}

// continuousWorkspace is the workspace identity of the root conversation an
// attached process uses: the explicit flag or the working directory.
func continuousWorkspace(args Args, r Resolved) string {
	if args.ContinuousWorkspace != "" {
		return args.ContinuousWorkspace
	}
	return r.CWD
}

// continuousDialer resolves the token and TLS settings once and returns a
// function that opens a new authenticated client session with the host.
// Swarm agents and the attached view each hold their own session.
func continuousDialer(args Args) (func(context.Context) (*continuous.Client, error), error) {
	token := args.ContinuousToken
	if args.ContinuousTokenFile != "" {
		data, err := os.ReadFile(args.ContinuousTokenFile)
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(string(data))
		if len(fields) == 0 {
			return nil, fmt.Errorf("token file %s is empty", args.ContinuousTokenFile)
		}
		token = fields[0]
	}
	var tlsConfig *tls.Config
	if args.ContinuousTLSCA != "" {
		var err error
		tlsConfig, err = continuous.ClientTLSConfig(args.ContinuousTLSCA)
		if err != nil {
			return nil, err
		}
	}
	address := args.Continuous
	return func(ctx context.Context) (*continuous.Client, error) {
		return continuous.DialTLS(ctx, address, token, tlsConfig)
	}, nil
}

func shortAddress(address string) string {
	if i := strings.LastIndex(address, "/"); i >= 0 && i < len(address)-1 {
		return address[i+1:]
	}
	return address
}
