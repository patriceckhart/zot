package telegram

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/internal/provider"
)

// Host is the small interface the Bridge calls back into the TUI
// through. Decouples bridge plumbing from the Interactive type.
type Host interface {
	// SubmitOrQueue feeds a user prompt into the running agent.
	// Runs now if the agent is idle, queues behind any in-flight
	// turn otherwise.
	SubmitOrQueue(prompt string, images []provider.ImageBlock)

	// CancelTurn aborts the active turn (if any). Called when the
	// paired Telegram user sends /stop.
	CancelTurn()

	// Notify pushes a one-shot status line into the chat. Used to
	// surface bridge events ("connected as @bot", "paired with
	// user X", etc.) in the user's local transcript.
	Notify(level, message string)
}

// Bridge polls Telegram for updates and forwards them into the
// Host's running agent, then mirrors the agent's final assistant
// text back to the paired Telegram user. One bridge per Interactive
// instance; created on /telegram connect, stopped on /telegram
// disconnect or zot exit.
type Bridge struct {
	Client *Client
	Config Config
	Save   func(Config) error
	Host   Host

	mu       sync.Mutex
	running  bool
	cancel   context.CancelFunc
	me       *User
	chatID   int64 // populated after first DM from the paired user
	replyBuf strings.Builder
}

// State is the snapshot /telegram status reports.
type State struct {
	Running  bool
	Username string // bot username, e.g. "zotbot"
	PairedID int64  // 0 when no user has claimed the bot yet
}

// Active reports whether the bridge is currently polling.
func (b *Bridge) Active() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// State returns a snapshot of the bridge for /telegram status.
func (b *Bridge) State() State {
	if b == nil {
		return State{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := State{Running: b.running, PairedID: b.Config.AllowedUserID}
	if b.me != nil {
		s.Username = b.me.Username
	}
	return s
}

// Start kicks off the polling loop. Idempotent: calling twice
// returns nil the second time and leaves the existing loop alone.
// Verifies the bot token with GetMe before starting the loop so
// obvious configuration errors surface immediately.
func (b *Bridge) Start(parent context.Context) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return nil
	}
	if b.Config.BotToken == "" {
		b.mu.Unlock()
		return fmt.Errorf("no bot token configured; run `zot telegram-bot setup` first")
	}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)
	// Quick handshake before committing the loop.
	me, err := b.Client.GetMe(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("getMe: %w", err)
	}

	b.mu.Lock()
	b.running = true
	b.cancel = cancel
	b.me = me
	if b.Config.BotID != me.ID || b.Config.BotUsername != me.Username {
		b.Config.BotID = me.ID
		b.Config.BotUsername = me.Username
		_ = b.Save(b.Config)
	}
	b.mu.Unlock()

	go b.pollLoop(ctx)
	return nil
}

// Stop halts the polling loop. Safe to call when not running.
func (b *Bridge) Stop() {
	b.mu.Lock()
	cancel := b.cancel
	b.running = false
	b.cancel = nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// OnAssistantText should be called by the TUI with the assistant's
// final visible text for each turn. The bridge forwards it to the
// paired chat in message-sized chunks. Safe to call from any
// goroutine; a no-op when the bridge is stopped or no chat is
// known yet.
func (b *Bridge) OnAssistantText(text string) {
	b.mu.Lock()
	chatID := b.chatID
	running := b.running
	b.mu.Unlock()
	if !running || chatID == 0 {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// Telegram caps at 4096 chars; chunk to be safe.
	for _, chunk := range chunkMessage(text, 4000) {
		if err := b.Client.SendMessage(context.Background(), chatID, chunk, 0); err != nil {
			fmt.Fprintln(stderr(), "telegram bridge: sendMessage:", err)
			return
		}
	}
}

// pollLoop long-polls Telegram and dispatches each update. Runs
// until ctx cancels.
func (b *Bridge) pollLoop(ctx context.Context) {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		updates, err := b.Client.GetUpdates(ctx, b.Config.LastUpdateID+1, 30)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			b.Host.Notify("warn", fmt.Sprintf("telegram: getUpdates: %v", err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			b.handleUpdate(ctx, u)
			b.mu.Lock()
			b.Config.LastUpdateID = u.UpdateID
			_ = b.Save(b.Config)
			b.mu.Unlock()
		}
	}
}

// handleUpdate applies pairing, gates on the allowed user, decodes
// the interesting bits (text, caption, image attachments), and
// forwards them to the Host. Built-in slash commands (/start,
// /help, /status, /stop) are handled inline without touching the
// agent.
func (b *Bridge) handleUpdate(ctx context.Context, u Update) {
	msg := u.Message
	if msg == nil {
		msg = u.Edited
	}
	if msg == nil || msg.From == nil || msg.From.IsBot {
		return
	}
	if msg.Chat.Type != "private" {
		return
	}

	text := strings.TrimSpace(msg.Text)

	// Pairing: first user who sends /start claims the bridge.
	b.mu.Lock()
	paired := b.Config.AllowedUserID
	b.mu.Unlock()
	if paired == 0 {
		if strings.HasPrefix(text, "/start") {
			b.mu.Lock()
			b.Config.AllowedUserID = msg.From.ID
			b.chatID = msg.Chat.ID
			_ = b.Save(b.Config)
			b.mu.Unlock()
			_ = b.Client.SendMessage(ctx, msg.Chat.ID,
				fmt.Sprintf("paired with @%s. messages you send here now mirror into the zot tui.", msg.From.Username),
				msg.MessageID)
			b.Host.Notify("success", fmt.Sprintf("telegram paired with user %d (@%s)", msg.From.ID, msg.From.Username))
			return
		}
		_ = b.Client.SendMessage(ctx, msg.Chat.ID,
			"this bot isn't paired yet. send /start to claim it.",
			msg.MessageID)
		return
	}

	if msg.From.ID != paired {
		_ = b.Client.SendMessage(ctx, msg.Chat.ID,
			"this bot is paired with a different user.",
			msg.MessageID)
		return
	}

	// Remember the chat id so replies can go out without waiting
	// for another update round-trip.
	b.mu.Lock()
	b.chatID = msg.Chat.ID
	b.mu.Unlock()

	// Built-in commands that bypass the agent.
	switch text {
	case "/start", "/help":
		_ = b.Client.SendMessage(ctx, msg.Chat.ID,
			"mirror is active. send me a message and it'll be forwarded to the zot tui. commands: /status, /stop.",
			msg.MessageID)
		return
	case "/status":
		_ = b.Client.SendMessage(ctx, msg.Chat.ID,
			fmt.Sprintf("mirror active. paired user: %d.", paired),
			msg.MessageID)
		return
	case "/stop":
		b.Host.CancelTurn()
		_ = b.Client.SendMessage(ctx, msg.Chat.ID,
			"cancelled the current turn.", msg.MessageID)
		return
	}

	// Build the prompt: text + caption; download image attachments.
	prompt := strings.TrimSpace(msg.Text)
	if msg.Caption != "" {
		if prompt != "" {
			prompt += "\n"
		}
		prompt += msg.Caption
	}

	var images []provider.ImageBlock
	if len(msg.Photo) > 0 {
		largest := msg.Photo[len(msg.Photo)-1]
		if data, mime, err := b.download(ctx, largest.FileID, ""); err == nil {
			images = append(images, provider.ImageBlock{MimeType: mime, Data: data})
		}
	}
	if msg.Document != nil && isImageMIME(msg.Document.MimeType) {
		if data, mime, err := b.download(ctx, msg.Document.FileID, msg.Document.MimeType); err == nil {
			images = append(images, provider.ImageBlock{MimeType: mime, Data: data})
		}
	}

	if prompt == "" && len(images) == 0 {
		return
	}

	b.Host.SubmitOrQueue(prompt, images)
}

// download fetches the file referenced by fileID and returns its
// bytes + mime type. Mime overrides the detected value when non-empty.
func (b *Bridge) download(ctx context.Context, fileID, mimeOverride string) ([]byte, string, error) {
	f, err := b.Client.GetFile(ctx, fileID)
	if err != nil {
		return nil, "", err
	}
	data, err := b.Client.DownloadFile(ctx, f.FilePath)
	if err != nil {
		return nil, "", err
	}
	mime := mimeOverride
	if mime == "" {
		mime = guessImageMIME(f.FilePath)
	}
	return data, mime, nil
}
