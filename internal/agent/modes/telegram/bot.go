package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/internal/core"
	"github.com/patriceckhart/zot/internal/provider"
)

// Bot owns the Telegram polling loop and dispatches inbound DMs to
// the agent. It is a long-running goroutine; Run blocks until ctx
// cancels.
type Bot struct {
	Client  *Client
	Agent   *core.Agent
	Config  Config
	ZotHome string
	// Save persists cfg to bot.json. Called whenever the bot pairs
	// with a new allowed user or advances LastUpdateID.
	Save func(Config) error
	// RefreshCreds is called before every turn to pick up newly
	// refreshed OAuth tokens. Optional; when nil, the bot uses the
	// credential it was built with. Implementations typically call
	// agent.ResolveCredentialFull which auto-refreshes expired tokens.
	RefreshCreds func() error

	mu        sync.Mutex
	busy      bool
	activeCtx context.CancelFunc
	queue     []queuedTurn
}

// queuedTurn is an inbound DM waiting to become a prompt.
type queuedTurn struct {
	chatID    int64
	messageID int
	prompt    string
	images    []provider.ImageBlock
}

// Run drives the bot. Returns when ctx is cancelled or GetMe fails.
func (b *Bot) Run(ctx context.Context) error {
	if b.Config.BotToken == "" {
		return fmt.Errorf("no bot token configured; run `zot bot setup` first")
	}
	me, err := b.Client.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("getMe: %w", err)
	}
	// Keep the stored username/id in sync with the actual bot.
	if b.Config.BotID != me.ID || b.Config.BotUsername != me.Username {
		b.Config.BotID = me.ID
		b.Config.BotUsername = me.Username
		_ = b.Save(b.Config)
	}

	fmt.Printf("telegram bridge online as @%s (id=%d)\n", me.Username, me.ID)
	if b.Config.AllowedUserID == 0 {
		fmt.Println("no user paired yet — send /start to the bot from Telegram to claim it")
	} else {
		fmt.Printf("paired with telegram user id %d\n", b.Config.AllowedUserID)
	}

	return b.pollLoop(ctx)
}

// pollLoop long-polls Telegram for updates and dispatches them.
func (b *Bot) pollLoop(ctx context.Context) error {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		updates, err := b.Client.GetUpdates(ctx, b.Config.LastUpdateID+1, 30)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintln(stderr(), "telegram: getUpdates error:", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			if err := b.handleUpdate(ctx, u); err != nil {
				fmt.Fprintln(stderr(), "telegram: handleUpdate:", err)
			}
			b.Config.LastUpdateID = u.UpdateID
			_ = b.Save(b.Config)
		}
	}
}

// handleUpdate processes a single Telegram update.
func (b *Bot) handleUpdate(ctx context.Context, u Update) error {
	msg := u.Message
	if msg == nil {
		msg = u.Edited
	}
	if msg == nil || msg.From == nil || msg.From.IsBot {
		return nil
	}
	if msg.Chat.Type != "private" {
		return nil
	}

	// Pairing: first user who sends /start claims the bridge.
	text := strings.TrimSpace(msg.Text)
	if b.Config.AllowedUserID == 0 {
		if strings.HasPrefix(text, "/start") {
			b.Config.AllowedUserID = msg.From.ID
			_ = b.Save(b.Config)
			_ = b.Client.SendMessage(ctx, msg.Chat.ID,
				fmt.Sprintf("paired with @%s. send any message and i'll forward it to zot.", msg.From.Username),
				msg.MessageID)
			return nil
		}
		_ = b.Client.SendMessage(ctx, msg.Chat.ID,
			"this bot isn't paired yet. send /start to claim it.",
			msg.MessageID)
		return nil
	}

	// Enforce allowed user.
	if msg.From.ID != b.Config.AllowedUserID {
		_ = b.Client.SendMessage(ctx, msg.Chat.ID,
			"this bot is paired with a different user.",
			msg.MessageID)
		return nil
	}

	// Built-in commands that bypass the agent.
	switch text {
	case "/start", "/help":
		_ = b.Client.SendMessage(ctx, msg.Chat.ID,
			"send me any message and i'll forward it to zot. attach an image and i'll pass it to the model. commands: /status, /stop.",
			msg.MessageID)
		return nil
	case "/status":
		return b.sendStatus(ctx, msg.Chat.ID, msg.MessageID)
	case "/stop":
		b.mu.Lock()
		cancel := b.activeCtx
		b.mu.Unlock()
		if cancel != nil {
			cancel()
			_ = b.Client.SendMessage(ctx, msg.Chat.ID, "cancelled the current turn.", msg.MessageID)
		} else {
			_ = b.Client.SendMessage(ctx, msg.Chat.ID, "nothing running.", msg.MessageID)
		}
		return nil
	}

	// Build the prompt: combine text + caption; download image attachments.
	prompt := strings.TrimSpace(msg.Text)
	if msg.Caption != "" {
		if prompt != "" {
			prompt += "\n"
		}
		prompt += msg.Caption
	}

	var images []provider.ImageBlock
	if len(msg.Photo) > 0 {
		// Photos arrive in multiple sizes; take the largest (last in the slice).
		largest := msg.Photo[len(msg.Photo)-1]
		if data, mime, err := b.download(ctx, largest.FileID, ""); err == nil {
			images = append(images, provider.ImageBlock{MimeType: mime, Data: data})
		} else {
			fmt.Fprintln(stderr(), "telegram: download photo:", err)
		}
	}
	if msg.Document != nil && isImageMIME(msg.Document.MimeType) {
		if data, mime, err := b.download(ctx, msg.Document.FileID, msg.Document.MimeType); err == nil {
			images = append(images, provider.ImageBlock{MimeType: mime, Data: data})
		}
	}

	if prompt == "" && len(images) == 0 {
		return nil
	}

	b.mu.Lock()
	b.queue = append(b.queue, queuedTurn{
		chatID:    msg.Chat.ID,
		messageID: msg.MessageID,
		prompt:    prompt,
		images:    images,
	})
	idle := !b.busy
	b.mu.Unlock()

	if idle {
		go b.drainQueue(ctx)
	}
	return nil
}

// drainQueue runs queued turns one at a time until the queue is empty.
func (b *Bot) drainQueue(parent context.Context) {
	for {
		b.mu.Lock()
		if len(b.queue) == 0 {
			b.busy = false
			b.activeCtx = nil
			b.mu.Unlock()
			return
		}
		t := b.queue[0]
		b.queue = b.queue[1:]
		b.busy = true
		turnCtx, cancel := context.WithCancel(parent)
		b.activeCtx = cancel
		b.mu.Unlock()

		if b.RefreshCreds != nil {
			if err := b.RefreshCreds(); err != nil {
				fmt.Fprintln(stderr(), "telegram: refresh creds:", err)
			}
		}
		b.runTurn(turnCtx, t)
		cancel()
	}
}

// runTurn sends the queued prompt to the agent and streams the reply.
func (b *Bot) runTurn(ctx context.Context, t queuedTurn) {
	stopTyping := b.startTyping(ctx, t.chatID)
	defer stopTyping()

	var replyBuilder strings.Builder
	var lastAssistantText string
	var turnErr error

	sink := func(ev core.AgentEvent) {
		switch e := ev.(type) {
		case core.EvTextDelta:
			replyBuilder.WriteString(e.Delta)
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

	if err := b.Agent.Prompt(ctx, t.prompt, t.images, sink); err != nil {
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
	// Telegram caps messages at 4096 chars. Chunk to be safe.
	for _, chunk := range chunkMessage(reply, 4000) {
		if err := b.Client.SendMessage(context.Background(), t.chatID, chunk, 0); err != nil {
			fmt.Fprintln(stderr(), "telegram: sendMessage:", err)
			break
		}
	}
}

// startTyping keeps Telegram's "typing..." indicator alive until the
// returned stop function is called.
func (b *Bot) startTyping(ctx context.Context, chatID int64) func() {
	tctx, cancel := context.WithCancel(ctx)
	go func() {
		for {
			_ = b.Client.SendChatAction(tctx, chatID, "typing")
			select {
			case <-tctx.Done():
				return
			case <-time.After(4 * time.Second):
			}
		}
	}()
	return cancel
}

// sendStatus describes agent state to the Telegram user.
func (b *Bot) sendStatus(ctx context.Context, chatID int64, replyTo int) error {
	b.mu.Lock()
	busy := b.busy
	queued := len(b.queue)
	b.mu.Unlock()

	state := "idle"
	if busy {
		state = "working"
	}
	lines := []string{
		fmt.Sprintf("state: %s", state),
		fmt.Sprintf("queued: %d", queued),
		fmt.Sprintf("model: %s", b.Agent.Model),
	}
	cost := b.Agent.Cost()
	lines = append(lines, fmt.Sprintf("cost: $%.4f (%d in / %d out)",
		cost.CostUSD, cost.InputTokens, cost.OutputTokens))
	return b.Client.SendMessage(ctx, chatID, strings.Join(lines, "\n"), replyTo)
}

// download fetches a file from Telegram and returns bytes + mime.
func (b *Bot) download(ctx context.Context, fileID, mime string) ([]byte, string, error) {
	f, err := b.Client.GetFile(ctx, fileID)
	if err != nil {
		return nil, "", err
	}
	data, err := b.Client.DownloadFile(ctx, f.FilePath)
	if err != nil {
		return nil, "", err
	}
	if mime == "" {
		mime = guessImageMIME(f.FilePath)
	}
	return data, mime, nil
}

// chunkMessage splits s into chunks no larger than limit runes, on line
// boundaries when possible.
func chunkMessage(s string, limit int) []string {
	if len(s) <= limit {
		return []string{s}
	}
	var out []string
	lines := strings.Split(s, "\n")
	var cur strings.Builder
	for _, l := range lines {
		if cur.Len()+len(l)+1 > limit && cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
		if len(l) > limit {
			// Line itself too long; hard-split.
			for len(l) > limit {
				out = append(out, l[:limit])
				l = l[limit:]
			}
		}
		if cur.Len() > 0 {
			cur.WriteString("\n")
		}
		cur.WriteString(l)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// isImageMIME returns true for MIME types the model can probably ingest
// as a vision input.
func isImageMIME(m string) bool {
	switch strings.ToLower(m) {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp":
		return true
	}
	return false
}

// guessImageMIME infers a mime type from a filename suffix. Falls back
// to image/png because telegram photos are always re-encoded to jpeg
// but getFile's file_path may omit the extension.
func guessImageMIME(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	}
	return "image/jpeg"
}
