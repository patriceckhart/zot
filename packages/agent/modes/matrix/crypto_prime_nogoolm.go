//go:build !goolm

package matrix

import (
	"context"

	"maunium.net/go/mautrix/id"
)

func (a *Adapter) primeEncryptedRoom(_ context.Context, _ id.RoomID, _ []id.UserID) error {
	return nil
}
