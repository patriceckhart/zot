//go:build windows

package agent

import "context"

// onReloadSignal has no signal on Windows; reload is available through the
// host protocol's runtime.reload method instead.
func onReloadSignal(ctx context.Context, fn func()) func() { return func() {} }
