// Package telegram implements zot's Telegram bot bridge.
//
// It runs in-process, polling Telegram for DMs and forwarding them to
// a core.Agent. Responses stream back as Telegram messages. Images
// sent by the user are downloaded and attached to the next prompt as
// provider.ImageBlock, so vision-capable models see them the same way
// they would via drag-and-drop in the TUI.
//
// State (bot token, allowed user id) lives in $ZOT_HOME/bot.json.
package telegram

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Config is the on-disk state for the telegram bridge.
type Config struct {
	BotToken      string `json:"bot_token,omitempty"`
	BotUsername   string `json:"bot_username,omitempty"`
	BotID         int64  `json:"bot_id,omitempty"`
	AllowedUserID int64  `json:"allowed_user_id,omitempty"`
	LastUpdateID  int64  `json:"last_update_id,omitempty"`
}

// ConfigPath returns the path to bot.json.
func ConfigPath(zotHome string) string {
	return filepath.Join(zotHome, "bot.json")
}

// LoadConfig reads bot.json, returning a zero Config if it doesn't exist.
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

// SaveConfig writes bot.json atomically.
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
