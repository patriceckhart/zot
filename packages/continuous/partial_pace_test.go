package continuous

import (
	"testing"
	"time"
)

// Partial commits are paced by size: small records wait the interval,
// large ones wait their size at partialBytesPerSecond, so rewriting a
// growing record cannot dominate the store's write volume.
func TestPartialDelayScalesWithSize(t *testing.T) {
	iv := 100 * time.Millisecond
	// Typical answers keep the plain interval.
	if d := partialDelay(iv, 40<<10); d != iv {
		t.Fatalf("40 KiB record: %v", d)
	}
	if d := partialDelay(iv, partialBytesPerSecond); d != time.Second {
		t.Fatalf("rate-sized record: %v", d)
	}
	if d := partialDelay(iv, MaxPartialBytes); d != 500*time.Millisecond {
		t.Fatalf("max record: %v", d)
	}
}

// The flush loop runs only when add signals new text: it does not poll on
// a ticker. The record is marked dirty without a signal; a polling loop
// would flush it, which panics here because the gate is nil.
func TestPartialLoopWakesOnlyOnText(t *testing.T) {
	p := &taskPartial{interval: time.Millisecond, wake: make(chan struct{}, 1), quit: make(chan struct{}), done: make(chan struct{})}
	p.dirty = true
	go p.loop()
	time.Sleep(20 * time.Millisecond)
	stopped := make(chan struct{})
	go func() { p.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop blocked")
	}
}
