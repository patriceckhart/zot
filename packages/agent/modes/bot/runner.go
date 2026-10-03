package bot

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// stderr is a tiny hook so tests can redirect bot logging.
var stderr = func() io.Writer { return os.Stderr }

// Config holds runner-level settings that are protocol-independent.
type Config struct {
	ZotHome      string
	Provider     string
	Model        string
	AuthMethod   string
	CWD          string
	RefreshCreds func() error
}

// queuedTurn is an inbound message waiting to become a prompt.
type queuedTurn struct {
	channelID string
	messageID string
	prompt    string
	images    []provider.ImageBlock
}

// Runner is the protocol-agnostic bot engine.  It owns the turn queue,
// dispatches prompts to the agent, and streams replies back through
// the BotAdapter.
type Runner struct {
	agent   *core.Agent
	adapter BotAdapter
	cfg     Config

	mu           sync.Mutex
	busy         bool
	activeCtx    context.CancelFunc
	queue        []queuedTurn
	lastCtxInput int
	runCtx       context.Context // set at Run entry; used by goroutines
}

// NewRunner creates a Runner wired to the given adapter and agent.
func NewRunner(adapter BotAdapter, agent *core.Agent, cfg Config) *Runner {
	return &Runner{
		agent:   agent,
		adapter: adapter,
		cfg:     cfg,
	}
}

// UpdateRuntimeConfig updates provider/model/auth/cwd at runtime (e.g. after
// credential refresh).  This is thread-safe.
func (r *Runner) UpdateRuntimeConfig(provider, model, authMethod, cwd string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.Provider = provider
	r.cfg.Model = model
	r.cfg.AuthMethod = authMethod
	r.cfg.CWD = cwd
}

// Run starts the adapter's polling loop and blocks until ctx cancels.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	r.runCtx = ctx
	r.mu.Unlock()

	return r.adapter.Run(ctx, r.handleMessage, r.handleCommand)
}

// handleMessage is called by the adapter for every normal inbound message.
func (r *Runner) handleMessage(msg InboundMessage) {
	if msg.Text == "" && len(msg.Images) == 0 {
		return
	}

	r.mu.Lock()
	r.queue = append(r.queue, queuedTurn{
		channelID: msg.ChannelID,
		messageID: msg.MessageID,
		prompt:    msg.Text,
		images:    msg.Images,
	})
	idle := !r.busy
	if idle {
		r.busy = true
	}
	r.mu.Unlock()

	if idle {
		go r.drainQueue()
	}
}

// handleCommand is called by the adapter for built-in commands.
func (r *Runner) handleCommand(cmd Command, msg InboundMessage) {
	switch cmd {
	case CmdStart, CmdHelp:
		_ = r.adapter.Send(context.Background(), msg.ChannelID,
			"send me any message and i'll forward it to zot. attach an image and i'll pass it to the model. commands: /status, /stop, or plain stop.",
			SendOptions{ReplyToMessageID: msg.MessageID})
	case CmdStatus:
		r.sendStatus(msg.ChannelID, msg.MessageID)
	case CmdStop:
		r.cancelActiveTurn(msg.ChannelID, msg.MessageID)
	}
}

// drainQueue runs queued turns one at a time until the queue is empty.
func (r *Runner) drainQueue() {
	r.mu.Lock()
	parent := r.runCtx
	r.mu.Unlock()

	for {
		r.mu.Lock()
		if len(r.queue) == 0 {
			r.busy = false
			r.activeCtx = nil
			r.mu.Unlock()
			return
		}
		t := r.queue[0]
		r.queue = r.queue[1:]
		turnCtx, cancel := context.WithCancel(parent)
		r.activeCtx = cancel
		r.mu.Unlock()

		if r.cfg.RefreshCreds != nil {
			if err := r.cfg.RefreshCreds(); err != nil {
				fmt.Fprintln(stderr(), "bot: refresh creds:", err)
			}
		}
		r.runTurn(turnCtx, t)
		cancel()
	}
}

// runTurn sends the queued prompt to the agent and streams the reply.
func (r *Runner) runTurn(ctx context.Context, t queuedTurn) {
	stopWorking := r.adapter.IndicateWorking(ctx, t.channelID)
	defer stopWorking()

	var replyBuilder strings.Builder
	var lastAssistantText string
	var turnErr error

	sink := func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvTextDelta:
			replyBuilder.WriteString(e.Delta)
		case core.EvUsage:
			r.mu.Lock()
			if !e.Auxiliary && e.Usage.InputTokens > 0 {
				r.lastCtxInput = e.Usage.InputTokens + e.Usage.CacheReadTokens + e.Usage.CacheWriteTokens
			}
			r.mu.Unlock()
		case core.EvAssistantMessage:
			var sb strings.Builder
			for _, c := range e.Message.Content {
				if tb, ok := c.(provider.TextBlock); ok {
					if sb.Len() > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(tb.Text)
				}
			}
			if sb.Len() > 0 {
				lastAssistantText = sb.String()
			}
			replyBuilder.Reset()
		case core.EvTurnEnd:
			if e.Err != nil {
				turnErr = e.Err
			}
		}
	}

	if err := r.agent.Prompt(ctx, t.prompt, t.images, sink); err != nil {
		turnErr = err
	}

	reply := strings.TrimSpace(lastAssistantText)
	if reply == "" {
		reply = strings.TrimSpace(replyBuilder.String())
	}
	if turnErr != nil && ctx.Err() == nil {
		reply = "error: " + turnErr.Error()
	}
	if reply == "" {
		reply = "(no reply)"
	}

	// Adapter.Send is responsible for chunking to protocol limits.
	if err := r.adapter.Send(context.Background(), t.channelID, reply, SendOptions{}); err != nil {
		fmt.Fprintln(stderr(), "bot: send reply:", err)
	}
}

// cancelActiveTurn aborts the currently running turn, if any.
func (r *Runner) cancelActiveTurn(channelID, messageID string) {
	r.mu.Lock()
	cancel := r.activeCtx
	r.mu.Unlock()
	opts := SendOptions{ReplyToMessageID: messageID}
	if cancel != nil {
		cancel()
		_ = r.adapter.Send(context.Background(), channelID, "cancelled the current turn.", opts)
	} else {
		_ = r.adapter.Send(context.Background(), channelID, "nothing running.", opts)
	}
}

// sendStatus describes agent state to the user.
func (r *Runner) sendStatus(channelID, messageID string) {
	r.mu.Lock()
	busy := r.busy
	queued := len(r.queue)
	ctxUsed := r.lastCtxInput
	providerName := r.cfg.Provider
	model := r.cfg.Model
	authMethod := r.cfg.AuthMethod
	cwd := r.cfg.CWD
	r.mu.Unlock()

	ctxMax := 0
	if m, err := provider.FindModel(providerName, model); err == nil {
		ctxMax = m.ContextWindow
	}

	status := FormatStatus(StatusSnapshot{
		Provider:     providerName,
		Model:        model,
		CWD:          cwd,
		Usage:        r.agent.Cost(),
		Subscription: authMethod == "oauth",
		ContextUsed:  ctxUsed,
		ContextMax:   ctxMax,
		Busy:         busy,
		Queued:       queued,
	})

	if extra := r.adapter.StatusText(); extra != "" {
		status += "\n" + extra
	}

	_ = r.adapter.Send(context.Background(), channelID, status, SendOptions{ReplyToMessageID: messageID})
}
