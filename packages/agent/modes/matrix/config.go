// Package matrix implements zot's Matrix bot bridge over mautrix-go.
//
// State (homeserver, access token, paired user, sync cursor) lives in
// $ZOT_HOME/matrix.json (0600). E2EE key material, when enabled via
// crypto_passphrase, lives in $ZOT_HOME/matrix-crypto/store.db.
package matrix

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config is the on-disk state for the matrix bridge.
type Config struct {
	Homeserver       string `json:"homeserver"`
	UserID           string `json:"user_id"`
	AccessToken      string `json:"access_token,omitempty"`
	DeviceID         string `json:"device_id,omitempty"`
	DisplayName      string `json:"display_name,omitempty"`
	AllowedUserID    string `json:"allowed_user_id,omitempty"` // "" = unpaired
	SinceToken       string `json:"since_token,omitempty"`     // sync cursor
	AutoJoin         bool   `json:"auto_join,omitempty"`
	GroupReplyAll    bool   `json:"group_reply_all,omitempty"`
	CryptoPassphrase string `json:"crypto_passphrase,omitempty"` // "" = no E2EE
	PairedRoomID     string `json:"paired_room_id,omitempty"`
}

// ConfigPath returns the path to matrix.json.
func ConfigPath(zotHome string) string { return filepath.Join(zotHome, "matrix.json") }

// LoadConfig reads matrix.json, returning a zero Config if it doesn't exist.
func LoadConfig(zotHome string) (Config, error) {
	var c Config
	b, err := os.ReadFile(ConfigPath(zotHome))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, nil
}

// SaveConfig writes matrix.json atomically, mode 0600.
func SaveConfig(zotHome string, c Config) error {
	if err := os.MkdirAll(zotHome, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	path := ConfigPath(zotHome)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// MaskToken masks an access token for status output. Access tokens
// are bearer credentials; only edges are shown.
func MaskToken(tok string) string {
	if len(tok) <= 10 {
		return "<hidden>"
	}
	return tok[:4] + "..." + tok[len(tok)-4:]
}
