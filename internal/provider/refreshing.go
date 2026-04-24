package provider

import (
	"context"
	"sync"
)

// TokenRefresher is a callback that checks whether the current token
// is still valid and returns a fresh one if needed. The returned
// string is the new access token; if empty the old one is still fine.
// An error means refresh failed (network down, refresh token expired,
// etc.) — the caller should proceed with the stale token and let the
// API return 401 naturally.
type TokenRefresher func(ctx context.Context) (newToken string, err error)

// RefreshingClient wraps a Client and calls a TokenRefresher before
// every Stream call. When the refresher returns a new token, a fresh
// underlying client is built via the factory function.
type RefreshingClient struct {
	mu      sync.Mutex
	inner   Client
	refresh TokenRefresher
	factory func(token string) Client
}

// NewRefreshingClient wraps inner with automatic token refresh.
// refreshFn is called before each Stream; if it returns a non-empty
// token the factory rebuilds the underlying client with the new token.
func NewRefreshingClient(inner Client, refreshFn TokenRefresher, factory func(token string) Client) *RefreshingClient {
	return &RefreshingClient{
		inner:   inner,
		refresh: refreshFn,
		factory: factory,
	}
}

func (c *RefreshingClient) Name() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inner.Name()
}

func (c *RefreshingClient) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	if c.refresh != nil {
		if newToken, err := c.refresh(ctx); err == nil && newToken != "" {
			c.mu.Lock()
			c.inner = c.factory(newToken)
			c.mu.Unlock()
		}
		// On refresh error: proceed with the current client.
		// The stale token will 401 and the user sees a clear error.
	}
	c.mu.Lock()
	inner := c.inner
	c.mu.Unlock()
	return inner.Stream(ctx, req)
}
