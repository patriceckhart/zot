package tools

import "context"

// MediaSender is the affordance protocol send-tools call into. The
// real implementation lives in the interactive runtime and forwards
// to the active bridge (telegram or matrix); tests can pass any stub.
type MediaSender interface {
	// SendImage uploads path as an inline-rendered image with an
	// optional caption.
	SendImage(ctx context.Context, path, caption string) error
	// SendDocument uploads path as a raw attachment.
	SendDocument(ctx context.Context, path, caption string) error
	// Active reports whether a paired chat is currently reachable.
	Active() bool
}

// TelegramSender is a back-compat alias; the interface was
// generalised when the Matrix bridge landed.
type TelegramSender = MediaSender
