//go:build !goolm

package matrix

import (
	"context"
	"fmt"
	"io"

	"maunium.net/go/mautrix"
)

// EnableCrypto is the no-crypto stub used when zot is built without
// the "goolm" build tag (the default CGO-free build). E2EE support
// requires the pure-Go olm implementation, which is opt-in:
//
//	CGO_ENABLED=0 go build -tags goolm ./...
//
// Without the tag, configuring a crypto_passphrase yields a clear
// error instead of a silent downgrade or a CGO dependency.
func EnableCrypto(_ context.Context, _ *mautrix.Client, _, passphrase string) (io.Closer, error) {
	if passphrase == "" {
		return nil, fmt.Errorf("crypto passphrase is empty")
	}
	return nil, fmt.Errorf("matrix E2EE requires building zot with -tags goolm (pure-Go olm); this binary was built without it")
}
