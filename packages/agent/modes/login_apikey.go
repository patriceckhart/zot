package modes

import "context"

// submitAPIKey keeps network validation off the input loop. Login completion
// uses the auth event channel, just like OAuth and the legacy browser form.
func (i *Interactive) submitAPIKey(provider, key string) {
	if i.cfg.AuthManager == nil {
		i.dialog.ShowResult(false, "authentication is unavailable")
		return
	}
	if i.dialog.cancelKey != nil {
		i.dialog.cancelKey()
	}
	parent := i.runCtx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	i.dialog.cancelKey = cancel
	go func() {
		defer cancel()
		_ = i.cfg.AuthManager.LoginAPIKey(ctx, provider, key)
	}()
}
