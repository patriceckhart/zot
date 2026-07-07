//go:build goolm

package matrix

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/id"
)

func (a *Adapter) primeEncryptedRoom(ctx context.Context, roomID id.RoomID, users []id.UserID) error {
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
