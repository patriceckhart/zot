package matrix

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/patriceckhart/zot/packages/agent/modes/bot"
	"github.com/patriceckhart/zot/packages/provider"
)

// Host is the small interface the Bridge calls back into the TUI
// through. Decouples bridge plumbing from the Interactive type.
type Host interface {
	// SubmitOrQueue feeds a user prompt into the running agent.
	// Runs now if the agent is idle, queues behind any in-flight
	// turn otherwise.
	SubmitOrQueue(prompt string, images []provider.ImageBlock)

	// CancelTurn aborts the active turn (if any). Called when the
	// paired Matrix user sends /stop or plain "stop".
	CancelTurn()

	// Status returns the current model, usage, context, and cwd summary
	// shown when the paired Matrix user sends /status.
	Status() string

	// Notify pushes a one-shot status line into the chat. Used to
	// surface bridge events ("connected as @bot", "paired with
	// user X", etc.) in the user's local transcript.
	Notify(level, message string)
}

// Bridge syncs with Matrix and forwards inbound DMs into the Host's
// running agent, then mirrors the agent's final assistant text back
// to the paired Matrix user. One bridge per Interactive instance;
// created on /matrix connect, stopped on /matrix disconnect or zot
// exit.
type Bridge struct {
	Config  Config
	Save    func(Config) error
	Host    Host
	ZotHome string

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	client  *mautrix.Client
	closer  io.Closer // crypto helper, nil without E2EE
	e2ee    bool
	// nextReplyFromMatrix mirrors telegram's nextReplyFromTelegram:
	// set when the in-flight turn originated from Matrix so the reply
	// is sent bare instead of prefixed.
	nextReplyFromMatrix bool
	memberCount         map[id.RoomID]int
	seen                map[id.EventID]bool
	seenOrder           []id.EventID
	startTime           time.Time
}

// State is the snapshot /matrix status reports.
type State struct {
	Running  bool
	UserID   string
	PairedID string // "" when unpaired
	E2EE     bool
}

// Active reports whether the bridge is currently syncing.
func (b *Bridge) Active() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// State returns a snapshot of the bridge for /matrix status.
func (b *Bridge) State() State {
	if b == nil {
		return State{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return State{
		Running:  b.running,
		UserID:   b.Config.UserID,
		PairedID: b.Config.AllowedUserID,
		E2EE:     b.e2ee,
	}
}

// Start kicks off the sync loop. Idempotent: calling twice returns
// nil the second time and leaves the existing loop alone. Verifies
// the access token with Whoami before starting the loop so obvious
// configuration errors surface immediately.
func (b *Bridge) Start(parent context.Context) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return nil
	}
	b.mu.Unlock()

	ctx, cancel := context.WithCancel(parent)

	cli, err := NewClient(b.ZotHome, &b.Config, b.Save)
	if err != nil {
		cancel()
		return err
	}
	userID, deviceID, err := Whoami(ctx, cli)
	if err != nil {
		cancel()
		return fmt.Errorf("whoami: %w", err)
	}
	if b.Config.UserID != userID || b.Config.DeviceID != deviceID {
		b.Config.UserID = userID
		b.Config.DeviceID = deviceID
		_ = b.Save(b.Config)
	}

	var closer io.Closer
	e2ee := false
	if b.Config.CryptoPassphrase != "" {
		c, cerr := EnableCrypto(ctx, cli, b.ZotHome, b.Config.CryptoPassphrase)
		if cerr != nil {
			cancel()
			return fmt.Errorf("e2ee init: %w", cerr)
		}
		closer = c
		e2ee = true
		b.Host.Notify("success", "matrix e2ee enabled")
	}

	b.mu.Lock()
	b.running = true
	b.cancel = cancel
	b.client = cli
	b.closer = closer
	b.e2ee = e2ee
	b.memberCount = map[id.RoomID]int{}
	b.seen = map[id.EventID]bool{}
	b.startTime = time.Now()
	b.mu.Unlock()

	go b.syncLoop(ctx)
	return nil
}

// Stop halts the sync loop. Safe to call when not running.
func (b *Bridge) Stop() {
	b.mu.Lock()
	cancel := b.cancel
	closer := b.closer
	b.running = false
	b.cancel = nil
	b.closer = nil
	b.client = nil
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if closer != nil {
		_ = closer.Close()
	}
}

// OnAssistantText should be called by the TUI with the assistant's
// final visible text for each turn. The bridge forwards it to the
// paired room in message-sized chunks. Prefix depends on which side
// initiated the turn: TUI-originated turns get "zot: " so the
// two-sided transcript reads naturally, while Matrix-originated
// turns send bare text.
func (b *Bridge) OnAssistantText(text string) {
	b.mu.Lock()
	prefix := ""
	if b.nextReplyFromMatrix {
		prefix = ""
		b.nextReplyFromMatrix = false
	}
	b.mu.Unlock()
	b.sendToPaired(text, prefix)
}

// OnUserTyped mirrors a message the user typed in the zot TUI into
// the paired Matrix room, tagged "you:" so the Matrix thread stays a
// complete record of the conversation.
func (b *Bridge) OnUserTyped(text string) {
	b.sendToPaired(text, "you: ")
}

// sendToPaired writes text (with an optional prefix, chunked to
// Matrix's ~4000-rune cap) to the paired room. No-op when the bridge
// is stopped or before the paired room is known.
func (b *Bridge) sendToPaired(text, prefix string) {
	b.mu.Lock()
	roomID := id.RoomID(b.Config.PairedRoomID)
	running := b.running
	cli := b.client
	b.mu.Unlock()
	if !running || roomID == "" || cli == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if prefix != "" {
		text = prefix + text
	}
	for _, chunk := range bot.ChunkMessage(text, 4000) {
		content := format.RenderMarkdown(chunk, true, false)
		if _, err := cli.SendMessageEvent(context.Background(), roomID, event.EventMessage, &content); err != nil {
			fmt.Fprintln(stderr(), "matrix bridge: send:", err)
			return
		}
	}
}

// SendImage uploads path to the paired Matrix room as an inline
// image. Returns an error if the bridge is not running, no room has
// paired yet, or the upload itself fails.
//
// TODO(e2ee-media): sending encrypted media to an encrypted room
// requires uploading via attachment.EncryptInPlace; plain uploads to
// encrypted rooms show as unencrypted attachments. Out of scope for
// the first cut.
func (b *Bridge) SendImage(ctx context.Context, path, caption string) error {
	b.mu.Lock()
	roomID := id.RoomID(b.Config.PairedRoomID)
	running := b.running
	cli := b.client
	b.mu.Unlock()
	if !running {
		return fmt.Errorf("matrix bridge is not running")
	}
	if roomID == "" {
		return fmt.Errorf("matrix bridge has no paired room yet")
	}
	if cli == nil {
		return fmt.Errorf("matrix bridge is not running")
	}
	uri, mime, size, err := UploadFile(ctx, cli, path)
	if err != nil {
		return err
	}
	content := event.MessageEventContent{
		MsgType: event.MsgImage,
		Body:    filepath.Base(path),
		URL:     uri.CUString(),
		Info:    &event.FileInfo{MimeType: mime, Size: size},
	}
	if caption != "" {
		content.Body = caption
	}
	_, err = cli.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
	return err
}

// SendDocument uploads path to the paired Matrix room as a raw file
// attachment (no compression). Counterpart of SendImage for the
// matrix_send_file tool.
//
// TODO(e2ee-media): see SendImage — encrypted rooms need encrypted uploads.
func (b *Bridge) SendDocument(ctx context.Context, path, caption string) error {
	b.mu.Lock()
	roomID := id.RoomID(b.Config.PairedRoomID)
	running := b.running
	cli := b.client
	b.mu.Unlock()
	if !running {
		return fmt.Errorf("matrix bridge is not running")
	}
	if roomID == "" {
		return fmt.Errorf("matrix bridge has no paired room yet")
	}
	if cli == nil {
		return fmt.Errorf("matrix bridge is not running")
	}
	uri, mime, size, err := UploadFile(ctx, cli, path)
	if err != nil {
		return err
	}
	content := event.MessageEventContent{
		MsgType: event.MsgFile,
		Body:    filepath.Base(path),
		URL:     uri.CUString(),
		Info:    &event.FileInfo{MimeType: mime, Size: size},
	}
	if caption != "" {
		content.Body = caption
	}
	_, err = cli.SendMessageEvent(ctx, roomID, event.EventMessage, &content)
	return err
}

// syncLoop runs the Matrix sync with backoff until ctx cancels.
// Errors are surfaced to the Host rather than stderr (the in-TUI
// flavour has a live transcript to notify).
func (b *Bridge) syncLoop(ctx context.Context) {
	syncer := b.client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		b.handleMessage(ctx, evt)
	})
	syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
		b.handleMember(ctx, evt)
	})

	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		err := b.client.SyncWithContext(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			b.Host.Notify("warn", fmt.Sprintf("matrix: sync: %v", err))
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
	}
}

// handleMember auto-joins invites addressed to us and invalidates the
// member-count cache for rooms whose membership changed.
func (b *Bridge) handleMember(ctx context.Context, evt *event.Event) {
	b.mu.Lock()
	delete(b.memberCount, evt.RoomID)
	b.mu.Unlock()

	content := evt.Content.AsMember()
	if content == nil || content.Membership != event.MembershipInvite {
		return
	}
	if evt.GetStateKey() != b.Config.UserID || !b.Config.AutoJoin {
		return
	}
	if _, err := b.client.JoinRoomByID(ctx, evt.RoomID); err != nil {
		b.Host.Notify("warn", fmt.Sprintf("matrix: auto-join: %v", err))
	}
}

// handleMessage applies dedup, staleness, gating, command parsing,
// and image download; then dispatches to the Host (inline commands)
// or forwards the prompt to the agent.
func (b *Bridge) handleMessage(ctx context.Context, evt *event.Event) {
	b.mu.Lock()
	if b.seen[evt.ID] {
		b.mu.Unlock()
		return
	}
	b.seen[evt.ID] = true
	b.seenOrder = append(b.seenOrder, evt.ID)
	if len(b.seenOrder) > 512 {
		delete(b.seen, b.seenOrder[0])
		b.seenOrder = b.seenOrder[1:]
	}
	start := b.startTime
	b.mu.Unlock()

	ts := time.UnixMilli(evt.Timestamp)
	if ts.Before(start.Add(-60 * time.Second)) {
		return
	}

	content := evt.Content.AsMessage()
	if content == nil {
		return
	}

	decision := gateInbound(
		b.Config.UserID, b.Config.AllowedUserID, evt.Sender.String(),
		b.members(ctx, evt.RoomID), content.Body, b.Config.DisplayName,
	)
	chanID := evt.RoomID.String()
	msgID := evt.ID.String()

	switch decision.action {
	case gateIgnore:
		return
	case gateReject:
		_ = sendMarkdown(ctx, b.client, chanID, "this bot is paired with a different user.", msgID)
		return
	case gateUnpaired:
		_ = sendMarkdown(ctx, b.client, chanID, "this bot isn't paired yet. DM it directly to claim it.", msgID)
		return
	case gatePair:
		b.mu.Lock()
		b.Config.AllowedUserID = evt.Sender.String()
		b.Config.PairedRoomID = chanID
		_ = b.Save(b.Config)
		b.mu.Unlock()
		_ = sendMarkdown(ctx, b.client, chanID,
			fmt.Sprintf("paired with %s. messages you send here now mirror into the zot tui.", evt.Sender), msgID)
		b.Host.Notify("success", "matrix paired with "+evt.Sender.String())
		return
	case gateAccept:
		// continue below
	}

	b.mu.Lock()
	if b.Config.PairedRoomID != chanID {
		b.Config.PairedRoomID = chanID
		_ = b.Save(b.Config)
	}
	b.mu.Unlock()

	// Built-in commands that bypass the agent.
	if cmd, ok := parseCommand(decision.body); ok {
		switch cmd {
		case bot.CmdStart, bot.CmdHelp:
			_ = sendMarkdown(ctx, b.client, chanID,
				"mirror is active. send me a message and it'll be forwarded to the zot tui. commands: /status, /stop, or plain stop.", msgID)
		case bot.CmdStatus:
			_ = sendMarkdown(ctx, b.client, chanID, b.Host.Status(), msgID)
		case bot.CmdStop:
			b.Host.CancelTurn()
			_ = sendMarkdown(ctx, b.client, chanID, "cancelled the current turn.", msgID)
		}
		return
	}

	var images []provider.ImageBlock
	prompt := decision.body
	if content.MsgType == event.MsgImage {
		if img, err := DownloadImage(ctx, b.client, content); err == nil {
			images = append(images, img)
		} else {
			b.Host.Notify("warn", fmt.Sprintf("matrix: download image: %v", err))
		}
	}

	if prompt == "" && len(images) == 0 {
		return
	}

	b.mu.Lock()
	b.nextReplyFromMatrix = true
	b.mu.Unlock()
	b.Host.SubmitOrQueue(prompt, images)
}

// members returns the joined-member count for a room, cached until a
// member event invalidates it. 0 on error (treated as group → quiet).
func (b *Bridge) members(ctx context.Context, roomID id.RoomID) int {
	b.mu.Lock()
	if n, ok := b.memberCount[roomID]; ok {
		b.mu.Unlock()
		return n
	}
	b.mu.Unlock()
	resp, err := b.client.JoinedMembers(ctx, roomID)
	if err != nil {
		b.Host.Notify("warn", fmt.Sprintf("matrix: joinedMembers: %v", err))
		return 0
	}
	n := len(resp.Joined)
	b.mu.Lock()
	b.memberCount[roomID] = n
	b.mu.Unlock()
	return n
}

// sendMarkdown renders text as markdown→HTML and delivers it, chunked
// to 4000 runes, with the first chunk threaded under replyTo.
func sendMarkdown(ctx context.Context, cli *mautrix.Client, channelID, text, replyTo string) error {
	roomID := id.RoomID(channelID)
	rt := id.EventID(replyTo)
	for _, chunk := range bot.ChunkMessage(text, 4000) {
		content := format.RenderMarkdown(chunk, true, false)
		if rt != "" {
			content.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: rt}}
			rt = ""
		}
		if _, err := cli.SendMessageEvent(ctx, roomID, event.EventMessage, &content); err != nil {
			return err
		}
	}
	return nil
}
