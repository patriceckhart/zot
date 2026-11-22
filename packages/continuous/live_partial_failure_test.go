package continuous

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

type previewReadStore struct {
	storage.Store
	started chan struct{}
	release chan struct{}
}

func (s *previewReadStore) Snapshot(ctx context.Context) (storage.Snapshot, error) {
	snap, err := s.Store.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return previewReadSnapshot{Snapshot: snap, store: s}, nil
}

type previewReadSnapshot struct {
	storage.Snapshot
	store *previewReadStore
}

func (s previewReadSnapshot) GetContext(ctx context.Context, key string) (json.RawMessage, bool, error) {
	close(s.store.started)
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-s.store.release:
		return storage.Get(ctx, s.Snapshot, key)
	}
}

func (s previewReadSnapshot) PageContext(ctx context.Context, prefix, after string, limit int) ([]storage.Record, error) {
	return storage.Page(ctx, s.Snapshot, prefix, after, limit)
}

func TestLivePreviewReadHonorsCancellation(t *testing.T) {
	r, p := livePartialFixture(t, time.Hour)
	s := &previewReadStore{Store: r.store, started: make(chan struct{}), release: make(chan struct{})}
	r.store = s
	defer close(s.release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := r.livePartial(ctx, p.base.ConversationID)
		done <- err
	}()
	select {
	case <-s.started:
	case <-time.After(time.Second):
		t.Fatal("preview never reached the checked read")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("preview read error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("preview read ignored watch cancellation")
	}
}

func TestLivePreviewDoesNotWaitForCheckpointLock(t *testing.T) {
	r, p := livePartialFixture(t, time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// commitScoped holds this mutex across storage I/O. A stalled write
	// must not hold the RAM preview path or prevent watch cancellation.
	p.tc.inv.mu.Lock()
	defer p.tc.inv.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, _, _, err := r.livePartial(ctx, p.base.ConversationID)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("RAM preview blocked on checkpoint I/O")
	}
}
