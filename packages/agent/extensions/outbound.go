package extensions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// orderedPipe serializes every host frame, not just notifications. Events
// enqueue without waiting for the subprocess; synchronous requests wait for
// their write. Overflow disconnects the consumer rather than silently dropping
// an event and continuing with a misleading audit stream.
type orderedPipe struct {
	pipe   io.WriteCloser
	mu     sync.Mutex
	queue  []outboundFrame
	bytes  int
	limit  int
	closed bool
	wake   chan struct{}
	done   chan struct{}
	once   sync.Once
}

type outboundFrame struct {
	data []byte
	ack  chan error
}

const outboundBytes = 16 * 1024 * 1024
const outboundFrames = 256

func newOrderedPipe(pipe io.WriteCloser) *orderedPipe {
	p := &orderedPipe{pipe: pipe, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go p.run()
	return p
}

func (p *orderedPipe) enqueue(data []byte, ack chan error) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return io.ErrClosedPipe
	}
	limit := p.limit
	if limit == 0 {
		limit = outboundBytes
	}
	if len(p.queue) >= outboundFrames || p.bytes+len(data) > limit {
		p.closed = true
		p.mu.Unlock()
		p.Close()
		return errors.New("extension outbound queue exceeded its limit")
	}
	p.queue = append(p.queue, outboundFrame{data: append([]byte(nil), data...), ack: ack})
	p.bytes += len(data)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}

func (p *orderedPipe) Write(data []byte) (int, error) {
	return p.writeTimeout(data, interceptTimeout)
}

func (p *orderedPipe) writeTimeout(data []byte, timeout time.Duration) (int, error) {
	return p.writeContext(context.Background(), data, timeout)
}

func (p *orderedPipe) writeContext(ctx context.Context, data []byte, timeout time.Duration) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if timeout <= 0 {
		p.Close()
		return 0, io.ErrClosedPipe
	}
	ack := make(chan error, 1)
	if err := p.enqueue(data, ack); err != nil {
		return 0, err
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-ack:
		if err != nil {
			return 0, err
		}
		return len(data), nil
	case <-p.done:
		return 0, io.ErrClosedPipe
	case <-ctx.Done():
		// A partially written frame cannot be safely withdrawn.
		p.Close()
		return 0, ctx.Err()
	case <-timer.C:
		p.Close()
		return 0, fmt.Errorf("extension outbound write timed out: %w", context.DeadlineExceeded)
	}
}

func (p *orderedPipe) Close() error {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.queue = nil
		p.bytes = 0
		p.mu.Unlock()
		close(p.done)
		_ = p.pipe.Close()
	})
	return nil
}

func (p *orderedPipe) run() {
	for {
		select {
		case <-p.done:
			return
		case <-p.wake:
		}
		for {
			p.mu.Lock()
			if p.closed || len(p.queue) == 0 {
				p.mu.Unlock()
				break
			}
			frame := p.queue[0]
			p.queue[0] = outboundFrame{}
			p.queue = p.queue[1:]
			p.bytes -= len(frame.data)
			p.mu.Unlock()
			n, err := p.pipe.Write(frame.data)
			if err == nil && n != len(frame.data) {
				err = io.ErrShortWrite
			}
			if frame.ack != nil {
				frame.ack <- err
			}
			if err != nil {
				p.Close()
				return
			}
		}
	}
}
