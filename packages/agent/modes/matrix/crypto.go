//go:build goolm

package matrix

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"

	_ "maunium.net/go/mautrix/crypto/goolm" // pure-Go olm — keeps the build CGO-free
	_ "modernc.org/sqlite"                  // CGO-free sqlite driver, registers "sqlite"
)

// EnableCrypto initialises the olm/megolm machine backed by a sqlite
// store at $ZOT_HOME/matrix-crypto/store.db. After a successful call
// the client transparently decrypts inbound m.room.encrypted events
// (re-dispatched as event.EventMessage) and encrypts outbound sends
// to encrypted rooms. Passphrase pickles the key store.
//
// Returns a closer the caller must Close on shutdown. Callers must
// only invoke this when a passphrase is configured; an empty
// passphrase is a hard error, not a silent no-crypto fallback.
//
// This implementation is compiled only with the "goolm" build tag so
// the default CGO-free build stays free of the olm crypto store.
// Build with: CGO_ENABLED=0 go build -tags goolm ./...
func EnableCrypto(ctx context.Context, cli *mautrix.Client, zotHome, passphrase string) (io.Closer, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("crypto passphrase is empty")
	}
	dbPath := CryptoStorePath(zotHome)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return nil, err
	}
	rawDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db, err := dbutil.NewWithDB(rawDB, "sqlite3")
	if err != nil {
		return nil, err
	}
	helper, err := initCryptoHelper(ctx, cli, passphrase, db)
	if err != nil {
		return nil, err
	}
	// mautrix-go's cryptohelper.Init does NOT attach itself to
	// cli.Crypto (the caller must, per the mautrix example). Without
	// this, SendMessageEvent silently skips encryption and inbound
	// m.room.encrypted events are not decrypted.
	cli.Crypto = helper
	if cli.Crypto == nil {
		return nil, fmt.Errorf("crypto helper did not attach to client")
	}
	return helper, nil
}

// notSharedErr is the error mautrix-go's cryptohelper returns from
// verifyDeviceKeysOnServer when the server already has our device keys
// but the local olm account isn't marked as shared — i.e. the crypto
// store got out of sync with the server (a crash between key upload and
// the shared-flag save, a reset that wiped store.db but left the old
// device's keys published, or a restored config paired with a fresh
// crypto DB). The keys on the server are ours and are correct, so the
// safe fix is to mark the local account shared and re-init, instead of
// failing the bot or forcing the user to delete the crypto store.
const notSharedErr = "olm account is not marked as shared, but there are keys on the server"

// initCryptoHelper creates and initialises the crypto helper, retrying
// once after re-publishing keys if the local olm account is out of sync
// with the server. This covers two states that both surface as the
// same mautrix-go error ("olm account is not marked as shared, but
// there are keys on the server"):
//
//   - the account row exists with shared=false (a crash between key
//     upload and the shared-flag save), or
//   - the crypto store was wiped but the device's keys are still
//     published on the server (e.g. `zot matrix-bot reset` removed
//     store.db while the device stayed registered).
//
// In both cases the local account is not marked shared. ShareKeys
// uploads the local identity/one-time keys (overwriting any stale ones
// on the server), marks the account shared, and persists it — which is
// the standard Matrix "lost my crypto store" recovery every client
// performs on a reset. Re-init then sees shared=true and matching keys
// and proceeds. Mirrors the recovery mautrix bridges (e.g. opencrow)
// apply for the same condition, generalised to handle the wiped-store
// case too.
func initCryptoHelper(ctx context.Context, cli *mautrix.Client, passphrase string, db *dbutil.Database) (*cryptohelper.CryptoHelper, error) {
	helper, err := cryptohelper.NewCryptoHelper(cli, []byte(passphrase), db)
	if err != nil {
		return nil, fmt.Errorf("create crypto helper: %w", err)
	}
	if err := helper.Init(ctx); err == nil {
		return helper, nil
	} else if !strings.Contains(err.Error(), notSharedErr) {
		return nil, fmt.Errorf("init crypto: %w", err)
	}

	fmt.Fprintln(stderr(), "matrix: crypto state mismatch (local account not marked shared but server has keys) — re-publishing keys")
	// helper.mach is set before verifyDeviceKeysOnServer runs, so
	// Machine() is safe to call after the failed Init.
	if err := helper.Machine().ShareKeys(ctx, -1); err != nil {
		return nil, fmt.Errorf("re-publish olm keys: %w", err)
	}

	// Re-init on the same helper: Init rebuilds mach, reloads the now-
	// shared account, and re-verifies against the server (keys now match).
	if err := helper.Init(ctx); err != nil {
		return nil, fmt.Errorf("init crypto after key re-publish: %w", err)
	}
	return helper, nil
}
