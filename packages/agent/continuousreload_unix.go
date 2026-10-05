//go:build !windows

package agent

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// onReloadSignal runs fn on SIGHUP until ctx ends. Returns a stop function.
func onReloadSignal(ctx context.Context, fn func()) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ch:
				fn()
			}
		}
	}()
	return func() {
		signal.Stop(ch)
		<-done
	}
}
