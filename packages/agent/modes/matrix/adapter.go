package matrix

import (
	"context"
	"fmt"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/id"

	"github.com/patriceckhart/zot/packages/agent/modes/bot"
	"github.com/patriceckhart/zot/packages/provider"
)

// Adapter implements bot.BotAdapter for Matrix. ChannelID is
// id.RoomID.String(); MessageID is id.EventID.String().
type Adapter struct {
	client  *mautrix.Client
	zotHome string
	cfg     *Config
	save    func(Config) error

	mu              sync.Mutex
	e2ee            bool
	memberCount     map[id.RoomID]int   // lazy cache; invalidated on member events
	encryptedPrimed map[id.RoomID]bool  // outbound megolm session shared after member refresh
	seen            map[id.EventID]bool // sync-replay dedup
	seenOrder       []id.EventID
	startTime       time.Time
}

// NewAdapter builds the standalone-daemon adapter from saved config.
func NewAdapter(zotHome string, cfg *Config, save func(Config) error) (*Adapter, error) {
	cli, err := NewClient(zotHome, cfg, save)
	if err != nil {
		return nil, err
	}
	return &Adapter{
		client:          cli,
		zotHome:         zotHome,
		cfg:             cfg,
		save:            save,
		memberCount:     map[id.RoomID]int{},
		encryptedPrimed: map[id.RoomID]bool{},
		seen:            map[id.EventID]bool{},
	}, nil
}

// Run verifies credentials, optionally initialises E2EE, registers
// sync handlers, and blocks in SyncWithContext until ctx cancels.
func (a *Adapter) Run(ctx context.Context,
	handler func(bot.InboundMessage),
	commandHandler func(bot.Command, bot.InboundMessage),
) error {
	userID, deviceID, err := Whoami(ctx, a.client)
	if err != nil {
		return fmt.Errorf("whoami: %w", err)
	}
	if a.cfg.UserID != userID || a.cfg.DeviceID != deviceID {
		a.cfg.UserID = userID
		a.cfg.DeviceID = deviceID
		_ = a.save(*a.cfg)
	}

	if a.cfg.CryptoPassphrase != "" {
		closer, cerr := EnableCrypto(ctx, a.client, a.zotHome, a.cfg.CryptoPassphrase)
		if cerr != nil {
			return fmt.Errorf("e2ee init: %w", cerr)
		}
		defer closer.Close()
		a.mu.Lock()
		a.e2ee = true
		a.mu.Unlock()
	} else {
		fmt.Fprintln(stderr(), "matrix: no crypto_passphrase configured — encrypted rooms will NOT work")
	}

	a.mu.Lock()
	a.startTime = time.Now()
	a.mu.Unlock()

	fmt.Printf("matrix bridge online as %s (device %s)\n", userID, deviceID)
	if a.cfg.AllowedUserID == "" {
		fmt.Println("no user paired yet — DM the bot from Matrix to claim it")
	} else {
		fmt.Printf("paired with %s\n", a.cfg.AllowedUserID)
	}

	syncer := a.client.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEventType(event.EventMessage, func(ctx context.Context, evt *event.Event) {
		a.handleMessage(ctx, evt, handler, commandHandler)
	})
	syncer.OnEventType(event.StateMember, func(ctx context.Context, evt *event.Event) {
		a.handleMember(ctx, evt)
	})

	// SyncWithContext returns on transient errors; loop with backoff
	// like telegram's pollLoop until ctx cancels.
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := a.client.SyncWithContext(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			fmt.Fprintln(stderr(), "matrix: sync error:", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// handleMember auto-joins invites addressed to us and invalidates
// the member-count cache for rooms whose membership changed.
func (a *Adapter) handleMember(ctx context.Context, evt *event.Event) {
	a.mu.Lock()
	delete(a.memberCount, evt.RoomID)
	a.mu.Unlock()

	content := evt.Content.AsMember()
	if content == nil || content.Membership != event.MembershipInvite {
		return
	}
	if evt.GetStateKey() != a.cfg.UserID || !a.cfg.AutoJoin {
		return
	}
	if _, err := a.client.JoinRoomByID(ctx, evt.RoomID); err != nil {
		fmt.Fprintln(stderr(), "matrix: auto-join:", err)
	}
}

// handleMessage applies dedup, staleness, gating, command parsing,
// and image download; then calls the generic callbacks.
func (a *Adapter) handleMessage(ctx context.Context, evt *event.Event,
	handler func(bot.InboundMessage),
	commandHandler func(bot.Command, bot.InboundMessage),
) {
	// Dedup: matrix sync can redeliver events around reconnects.
	a.mu.Lock()
	if a.seen[evt.ID] {
		a.mu.Unlock()
		return
	}
	a.seen[evt.ID] = true
	a.seenOrder = append(a.seenOrder, evt.ID)
	if len(a.seenOrder) > 512 {
		delete(a.seen, a.seenOrder[0])
		a.seenOrder = a.seenOrder[1:]
	}
	start := a.startTime
	a.mu.Unlock()

	// Old-message skip: don't fire stale prompts while catching up.
	ts := time.UnixMilli(evt.Timestamp)
	if ts.Before(start.Add(-60 * time.Second)) {
		return
	}

	content := evt.Content.AsMessage()
	if content == nil {
		return
	}

	decision := gateInbound(
		a.cfg.UserID, a.cfg.AllowedUserID, evt.Sender.String(),
		a.members(ctx, evt.RoomID), content.Body, a.cfg.DisplayName,
	)
	chanID := evt.RoomID.String()
	msgID := evt.ID.String()

	switch decision.action {
	case gateIgnore:
		return
	case gateReject:
		_ = a.Send(ctx, chanID, "this bot is paired with a different user.", bot.SendOptions{ReplyToMessageID: msgID})
		return
	case gateUnpaired:
		_ = a.Send(ctx, chanID, "this bot isn't paired yet. DM it directly to claim it.", bot.SendOptions{ReplyToMessageID: msgID})
		return
	case gatePair:
		a.cfg.AllowedUserID = evt.Sender.String()
		a.cfg.PairedRoomID = chanID
		_ = a.save(*a.cfg)
		_ = a.Send(ctx, chanID,
			fmt.Sprintf("paired with %s. send any message and i'll forward it to zot.", evt.Sender),
			bot.SendOptions{ReplyToMessageID: msgID})
		// A pairing message that is also a command/prompt: fall through
		// so the first real prompt isn't swallowed — but a bare /start
		// analogue ("hi") shouldn't double-reply, so stop here.
		return
	case gateAccept:
		// continue below
	}

	// Remember the paired room so the in-TUI bridge can send without
	// waiting for a fresh DM.
	if a.cfg.PairedRoomID != chanID {
		a.cfg.PairedRoomID = chanID
		_ = a.save(*a.cfg)
	}

	inbound := bot.InboundMessage{ChannelID: chanID, MessageID: msgID}

	if cmd, ok := parseCommand(decision.body); ok {
		commandHandler(cmd, inbound)
		return
	}

	var images []provider.ImageBlock
	prompt := decision.body
	if content.MsgType == event.MsgImage {
		if img, err := DownloadImage(ctx, a.client, content); err == nil {
			images = append(images, img)
			// For image events Body is the filename/caption; treat it
			// as the caption text like telegram does.
		} else {
			fmt.Fprintln(stderr(), "matrix: download image:", err)
		}
	}

	inbound.Text = prompt
	inbound.Images = images
	handler(inbound)
}

// members returns the joined-member count for a room, cached until a
// member event invalidates it. 0 on error (treated as group → quiet).
func (a *Adapter) members(ctx context.Context, roomID id.RoomID) int {
	a.mu.Lock()
	if n, ok := a.memberCount[roomID]; ok {
		a.mu.Unlock()
		return n
	}
	a.mu.Unlock()
	resp, err := a.client.JoinedMembers(ctx, roomID)
	if err != nil {
		fmt.Fprintln(stderr(), "matrix: joinedMembers:", err)
		return 0
	}
	n := len(resp.Joined)
	a.mu.Lock()
	a.memberCount[roomID] = n
	a.mu.Unlock()
	return n
}

// Send renders text as markdown→HTML and delivers it, chunked to
// 4000 runes, with the first chunk threaded under ReplyToMessageID.
// Encryption is transparent via client.Crypto when enabled.
func (a *Adapter) Send(ctx context.Context, channelID, text string, opts bot.SendOptions) error {
	roomID := id.RoomID(channelID)
	if err := a.ensureEncryptedSendReady(ctx, roomID); err != nil {
		return err
	}
	replyTo := id.EventID(opts.ReplyToMessageID)
	for _, chunk := range bot.ChunkMessage(text, 4000) {
		content := format.RenderMarkdown(chunk, true, false)
		if replyTo != "" {
			content.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: replyTo}}
			replyTo = "" // only the first chunk threads
		}
		if _, err := a.client.SendMessageEvent(ctx, roomID, event.EventMessage, &content); err != nil {
			return err
		}
	}
	return nil
}

// ensureEncryptedSendReady refreshes room/member state before the first
// encrypted send in a room and shares a fresh outbound megolm session to
// all joined members. This avoids the first pairing reply racing ahead
// of state sync after an auto-join, which otherwise creates an encrypted
// event whose session wasn't shared to the user's Element device.
func (a *Adapter) ensureEncryptedSendReady(ctx context.Context, roomID id.RoomID) error {
	if a.client.Crypto == nil || a.client.StateStore == nil {
		return nil
	}
	encrypted, err := a.client.StateStore.IsEncrypted(ctx, roomID)
	if err != nil {
		var enc event.EncryptionEventContent
		if serr := a.client.StateEvent(ctx, roomID, event.StateEncryption, "", &enc); serr != nil {
			return fmt.Errorf("check matrix encryption state: %w", err)
		}
		if err := a.client.StateStore.SetEncryptionEvent(ctx, roomID, &enc); err != nil {
			return fmt.Errorf("cache matrix encryption state: %w", err)
		}
		encrypted = true
	}
	if !encrypted {
		return nil
	}

	joined, err := a.client.JoinedMembers(ctx, roomID)
	if err != nil {
		return fmt.Errorf("refresh matrix room members: %w", err)
	}
	users := make([]id.UserID, 0, len(joined.Joined))
	for userID, member := range joined.Joined {
		users = append(users, userID)
		if err := a.client.StateStore.SetMember(ctx, roomID, userID, &event.MemberEventContent{
			Membership:  event.MembershipJoin,
			Displayname: member.DisplayName,
			AvatarURL:   id.ContentURIString(member.AvatarURL),
		}); err != nil {
			return fmt.Errorf("cache matrix room member %s: %w", userID, err)
		}
	}
	if err := a.client.StateStore.MarkMembersFetched(ctx, roomID); err != nil {
		return fmt.Errorf("mark matrix members fetched: %w", err)
	}

	a.mu.Lock()
	primed := a.encryptedPrimed[roomID]
	if !primed {
		a.encryptedPrimed[roomID] = true
	}
	a.mu.Unlock()
	if primed {
		return nil
	}

	helper, ok := a.client.Crypto.(*cryptohelper.CryptoHelper)
	if !ok {
		return nil
	}
	// Drop any outbound megolm session that may have been created before
	// the member list was cached (or by a previous buggy build). The next
	// ShareGroupSession call creates a fresh session and sends it to all
	// current members' devices.
	_ = helper.Machine().CryptoStore.RemoveOutboundGroupSession(ctx, roomID)
	if err := helper.Machine().ShareGroupSession(ctx, roomID, users); err != nil {
		return fmt.Errorf("share matrix encryption session: %w", err)
	}
	return nil
}

// IndicateWorking keeps the m.typing indicator alive until stop is
// called: refresh a 30s timeout every 25s, clear it on stop.
func (a *Adapter) IndicateWorking(ctx context.Context, channelID string) (stop func()) {
	roomID := id.RoomID(channelID)
	tctx, cancel := context.WithCancel(ctx)
	go func() {
		for {
			_, _ = a.client.UserTyping(tctx, roomID, true, 30*time.Second)
			select {
			case <-tctx.Done():
				_, _ = a.client.UserTyping(context.Background(), roomID, false, 0)
				return
			case <-time.After(25 * time.Second):
			}
		}
	}()
	return cancel
}

// StatusText reports the bot identity + encryption state for /status.
func (a *Adapter) StatusText() string {
	a.mu.Lock()
	e2ee := a.e2ee
	a.mu.Unlock()
	tag := "(no e2ee)"
	if e2ee {
		tag = "(e2ee)"
	}
	s := a.cfg.UserID
	if a.cfg.DisplayName != "" {
		s += " (" + a.cfg.DisplayName + ")"
	}
	return s + " " + tag
}

// Compile-time interface assertion.
var _ bot.BotAdapter = (*Adapter)(nil)
