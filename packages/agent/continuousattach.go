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
// onStatus receives waiting-state descriptions of followed submissions; it
// may be nil. The returned notice is non-empty when the loaded transcript
// is only the newest page of the conversation.
func attachContinuous(ctx context.Context, args Args, r Resolved, ag *core.Agent, onStatus func(string)) (func(context.Context, *core.Agent, string, func(core.AgentEvent)) error, string, string, func(), error) {
	dial, err := continuousDialer(args)
	if err != nil {
		return nil, "", "", nil, err
	}
	client, err := dial(ctx)
	if err != nil {
		return nil, "", "", nil, err
	}
	workspace := continuousWorkspace(args, r)
	// Local defaults and credentials are unrelated to the host. Only explicit
	// CLI selections override its defaults when creating a new root.
	config := continuous.AgentConfig{Reasoning: args.Reasoning}
	if args.Provider != "" {
		config.Provider = canonicalProvider(args.Provider)
	}
	if args.Model != "" {
		config.Model = r.Model
	}
	var conv continuous.Conversation
	if err := client.CallInto(ctx, "conversation.create", map[string]any{"workspace": workspace, "config": config}, &conv); err != nil {
		client.Close()
		return nil, "", "", nil, fmt.Errorf("open workspace conversation on host: %w", err)
	}
	driver := &continuous.AttachedDriver{Client: client, ConversationID: conv.ID, OnStatus: onStatus}
	notice := ""
	if ag != nil {
		snap, err := driver.Load(ctx, ag)
		if err != nil {
			client.Close()
			return nil, "", "", nil, err
		}
		notice = continuous.HistoryNotice(snap, conv.ID)
	}
	label := "attached: " + shortAddress(args.Continuous)
	return driver.Prompt, label, notice, func() { client.Close() }, nil
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
