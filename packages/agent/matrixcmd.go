package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/patriceckhart/zot/packages/agent/modes/bot"
	"github.com/patriceckhart/zot/packages/agent/modes/matrix"
)

func init() {
	botSpecs = append(botSpecs, matrixSpec())
}

func matrixSpec() *botSpec {
	return &botSpec{
		name:       "matrix",
		subcommand: "matrix-bot",
		aliases:    []string{"mx"},
		pidPath:    matrix.PIDPath,
		logPath:    matrix.LogPath,
		configured: func(zotHome string) (bool, error) {
			cfg, err := matrix.LoadConfig(zotHome)
			if err != nil {
				return false, err
			}
			return cfg.AccessToken != "", nil
		},
		printHelp: printMatrixBotHelp,
		setup:     matrixBotSetup,
		status:    matrixBotStatus,
		reset:     matrixBotReset,
		newAdapter: func(zotHome string) (bot.BotAdapter, error) {
			cfg, err := matrix.LoadConfig(zotHome)
			if err != nil {
				return nil, err
			}
			return matrix.NewAdapter(zotHome, &cfg,
				func(c matrix.Config) error { return matrix.SaveConfig(zotHome, c) })
		},
	}
}

func printMatrixBotHelp() {
	fmt.Fprint(os.Stderr, `zot matrix-bot — matrix bridge

usage:
  zot matrix-bot setup            homeserver + token or password login, verify, save
  zot matrix-bot status           show bridge config (token masked) and process state
  zot matrix-bot run [flags]      run in the foreground (ctrl+c to stop)
  zot matrix-bot start [flags]    launch in background, detach, return immediately
  zot matrix-bot stop             sigterm the running background bot
  zot matrix-bot logs [--follow]  tail the background bot's log file
  zot matrix-bot reset            forget credentials + E2EE key store

setup flow:
  1. run "zot matrix-bot setup" — enter your homeserver URL, then either
     paste an access token (Element → Settings → Help → Access Token)
     or log in with username + password
  2. optionally set a crypto passphrase to enable E2EE (needed for
     encrypted DMs — most clients encrypt DMs by default)
  3. run "zot matrix-bot start" (background) or "run" (foreground)
  4. invite the bot to a DM from your client; the first DM sender claims it

security: the access token is a bearer credential stored 0600 in
matrix.json. if it leaks, log the device out from your client and run
setup again.

config & state:
  $ZOT_HOME/matrix.json             credentials + paired user (0600)
  $ZOT_HOME/matrix.pid              pid of the running bot
  $ZOT_HOME/logs/matrix.log         stdout+stderr from "start"
  $ZOT_HOME/matrix-crypto/store.db  E2EE keys (only with a passphrase)

E2EE: build zot with -tags goolm to enable end-to-end encryption
(pure-Go olm, no CGO). without the tag, encrypted rooms are unreadable
and the bot errors out when a crypto passphrase is configured.
`)
}

// matrixBotSetup interactively collects homeserver + credentials,
// verifies via whoami, and persists matrix.json.
func matrixBotSetup(_ []string) error {
	reader := bufio.NewReader(os.Stdin)
	readLine := func(prompt string) (string, error) {
		fmt.Print(prompt)
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(line), nil
	}

	homeserver, err := readLine("homeserver URL (e.g. https://matrix.org): ")
	if err != nil {
		return err
	}
	if !strings.HasPrefix(homeserver, "http") {
		return fmt.Errorf("homeserver must be a URL, got %q", homeserver)
	}

	token, err := readLine("access token (leave empty for username+password login): ")
	if err != nil {
		return err
	}

	cfg := matrix.Config{Homeserver: homeserver, AutoJoin: true}
	ctx := context.Background()

	if token == "" {
		username, err := readLine("username (localpart or full @user:server): ")
		if err != nil {
			return err
		}
		password, err := readLine("password: ")
		if err != nil {
			return err
		}
		userID, accessToken, deviceID, err := matrix.Login(ctx, homeserver, username, password)
		if err != nil {
			return fmt.Errorf("login failed: %w", err)
		}
		cfg.UserID, cfg.AccessToken, cfg.DeviceID = userID, accessToken, deviceID
	} else {
		cfg.AccessToken = token
		cli, err := matrix.NewClientForSetup(homeserver, token)
		if err != nil {
			return err
		}
		userID, deviceID, err := matrix.Whoami(ctx, cli)
		if err != nil {
			return fmt.Errorf("token rejected by homeserver: %w", err)
		}
		cfg.UserID, cfg.DeviceID = userID, deviceID
	}

	pass, err := readLine("E2EE passphrase (leave empty to disable encryption support): ")
	if err != nil {
		return err
	}
	cfg.CryptoPassphrase = pass

	if err := matrix.SaveConfig(ZotHome(), cfg); err != nil {
		return err
	}
	fmt.Printf("\nsaved: %s (device %s) to %s\n", cfg.UserID, cfg.DeviceID, matrix.ConfigPath(ZotHome()))
	if pass == "" {
		fmt.Println("warning: no E2EE passphrase — the bot cannot read encrypted rooms.")
	}
	fmt.Println("next: run `zot matrix-bot run`, then DM the bot from Matrix; the first sender claims it.")
	return nil
}

func matrixBotStatus() error {
	cfg, err := matrix.LoadConfig(ZotHome())
	if err != nil {
		return err
	}
	if cfg.AccessToken == "" {
		fmt.Println("matrix: not configured (run `zot matrix-bot setup`)")
		return nil
	}
	fmt.Printf("matrix bot:   %s (device %s)\n", cfg.UserID, cfg.DeviceID)
	fmt.Printf("homeserver:   %s\n", cfg.Homeserver)
	fmt.Printf("token:        %s\n", matrix.MaskToken(cfg.AccessToken))
	if cfg.CryptoPassphrase != "" {
		fmt.Println("e2ee:         enabled")
	} else {
		fmt.Println("e2ee:         disabled (no crypto_passphrase)")
	}
	if cfg.AllowedUserID == "" {
		fmt.Println("paired with:  (unpaired — DM the bot from Matrix to claim)")
	} else {
		fmt.Printf("paired with:  %s\n", cfg.AllowedUserID)
	}
	fmt.Printf("config file:  %s\n", matrix.ConfigPath(ZotHome()))

	pid, alive, _ := matrix.IsRunning(ZotHome())
	switch {
	case alive:
		fmt.Printf("process:      running (pid %d)\n", pid)
	case pid > 0:
		fmt.Printf("process:      stopped (stale pid %d in %s)\n", pid, matrix.PIDPath(ZotHome()))
	default:
		fmt.Println("process:      stopped")
	}
	if fi, err := os.Stat(matrix.LogPath(ZotHome())); err == nil {
		fmt.Printf("log file:     %s (%d bytes)\n", matrix.LogPath(ZotHome()), fi.Size())
	}
	return nil
}

// matrixBotReset wipes matrix.json and the E2EE key store.
func matrixBotReset() error {
	removed := false
	if p := matrix.ConfigPath(ZotHome()); fileExists(p) {
		if err := os.Remove(p); err != nil {
			return err
		}
		fmt.Println("removed", p)
		removed = true
	}
	cryptoDir := filepath.Dir(matrix.CryptoStorePath(ZotHome()))
	if fileExists(cryptoDir) {
		if err := os.RemoveAll(cryptoDir); err != nil {
			return err
		}
		fmt.Println("removed", cryptoDir)
		removed = true
	}
	if !removed {
		fmt.Println("no matrix config to reset")
	}
	fmt.Println("note: also log the bot's device out from your Matrix client to invalidate the token.")
	return nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
