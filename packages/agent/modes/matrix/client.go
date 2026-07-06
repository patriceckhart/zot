package matrix

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/patriceckhart/zot/packages/agent/modes/bot"
	"github.com/patriceckhart/zot/packages/provider"
)

// NewClient builds a mautrix client from saved config. The sync
// cursor persists through cfgStore so restarts resume where they
// stopped (the analogue of Telegram's last_update_id).
func NewClient(zotHome string, cfg *Config, save func(Config) error) (*mautrix.Client, error) {
	if cfg.Homeserver == "" || cfg.UserID == "" || cfg.AccessToken == "" {
		return nil, fmt.Errorf("matrix is not configured — run `zot matrix-bot setup` first")
	}
	cli, err := mautrix.NewClient(cfg.Homeserver, id.UserID(cfg.UserID), cfg.AccessToken)
	if err != nil {
		return nil, err
	}
	cli.DeviceID = id.DeviceID(cfg.DeviceID)
	cli.Store = &cfgSyncStore{cfg: cfg, save: save}
	return cli, nil
}

// NewClientForSetup builds an unconfigured client for token
// verification during setup (no sync store, no config).
func NewClientForSetup(homeserver, token string) (*mautrix.Client, error) {
	return mautrix.NewClient(homeserver, "", token)
}

// cfgSyncStore persists the next-batch token into matrix.json.
// Implements mautrix.SyncStore.
type cfgSyncStore struct {
	cfg  *Config
	save func(Config) error
}

func (s *cfgSyncStore) SaveFilterID(_ context.Context, _ id.UserID, _ string) error {
	return nil
}
func (s *cfgSyncStore) LoadFilterID(_ context.Context, _ id.UserID) (string, error) {
	return "", nil
}
func (s *cfgSyncStore) SaveNextBatch(_ context.Context, _ id.UserID, next string) error {
	s.cfg.SinceToken = next
	return s.save(*s.cfg)
}
func (s *cfgSyncStore) LoadNextBatch(_ context.Context, _ id.UserID) (string, error) {
	return s.cfg.SinceToken, nil
}

// Login performs a password login and returns the credentials to persist.
func Login(ctx context.Context, homeserver, username, password string) (userID, accessToken, deviceID string, err error) {
	cli, err := mautrix.NewClient(homeserver, "", "")
	if err != nil {
		return "", "", "", err
	}
	resp, err := cli.Login(ctx, &mautrix.ReqLogin{
		Type:             mautrix.AuthTypePassword,
		Identifier:       mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: username},
		Password:         password,
		StoreCredentials: false,
	})
	if err != nil {
		return "", "", "", err
	}
	return resp.UserID.String(), resp.AccessToken, resp.DeviceID.String(), nil
}

// Whoami verifies the stored token and returns the canonical
// user id + device id (needed for E2EE).
func Whoami(ctx context.Context, cli *mautrix.Client) (userID, deviceID string, err error) {
	resp, err := cli.Whoami(ctx)
	if err != nil {
		return "", "", err
	}
	return resp.UserID.String(), resp.DeviceID.String(), nil
}

// UploadFile uploads a local file to the homeserver's media repo.
func UploadFile(ctx context.Context, cli *mautrix.Client, path string) (id.ContentURI, string, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return id.ContentURI{}, "", 0, err
	}
	mime := bot.GuessImageMIME(path)
	if !strings.HasPrefix(mime, "image/") || !bot.IsImageMIME(mime) {
		mime = "application/octet-stream"
	}
	resp, err := cli.UploadMedia(ctx, mautrix.ReqUploadMedia{
		ContentBytes: data,
		ContentType:  mime,
		FileName:     filepath.Base(path),
	})
	if err != nil {
		return id.ContentURI{}, "", 0, err
	}
	return resp.ContentURI, mime, len(data), nil
}

// DownloadImage fetches an m.image event's media (plain or E2EE)
// and returns it as a provider.ImageBlock for the model.
func DownloadImage(ctx context.Context, cli *mautrix.Client, content *event.MessageEventContent) (provider.ImageBlock, error) {
	mime := ""
	if content.Info != nil {
		mime = content.Info.MimeType
	}
	var data []byte
	switch {
	case content.File != nil: // encrypted media
		uri, err := content.File.URL.Parse()
		if err != nil {
			return provider.ImageBlock{}, err
		}
		data, err = cli.DownloadBytes(ctx, uri)
		if err != nil {
			return provider.ImageBlock{}, err
		}
		if err := content.File.DecryptInPlace(data); err != nil {
			return provider.ImageBlock{}, fmt.Errorf("decrypt media: %w", err)
		}
	case content.URL != "":
		uri, err := content.URL.Parse()
		if err != nil {
			return provider.ImageBlock{}, err
		}
		data, err = cli.DownloadBytes(ctx, uri)
		if err != nil {
			return provider.ImageBlock{}, err
		}
	default:
		return provider.ImageBlock{}, fmt.Errorf("image event has no media url")
	}
	if mime == "" {
		mime = "image/png"
	}
	if !bot.IsImageMIME(mime) {
		return provider.ImageBlock{}, fmt.Errorf("unsupported image mime %q", mime)
	}
	return provider.ImageBlock{MimeType: mime, Data: data}, nil
}
