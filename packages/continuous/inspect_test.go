package continuous

import (
	"context"
	"errors"
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
)

func TestInspectPagesAndStatus(t *testing.T) {
	r := newTestRuntime(t)
	ctx := context.Background()
	for _, workspace := range []string{"one", "two", "three"} {
		if _, err := r.OpenRoot(ctx, workspace, AgentConfig{}); err != nil {
			t.Fatal(err)
		}
	}
	status, err := r.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.ExecutionEnabled || status.Capabilities.Durability != storage.Memory || status.WriterEpoch == 0 {
		t.Fatalf("status: %+v", status)
	}
	after := ""
	ids := make(map[string]bool)
	for i := 0; i < 3; i++ {
		conversations, revision, err := r.Conversations(ctx, after, 1)
		if err != nil || len(conversations) != 1 || revision != status.Revision {
			t.Fatalf("page: %+v %d %v", conversations, revision, err)
		}
		id := conversations[0].ID
		if ids[id] || id <= after {
			t.Fatal("non-monotonic page")
		}
		ids[id] = true
		after = id
	}
	empty, _, err := r.Conversations(ctx, after, 1)
	if err != nil || len(empty) != 0 {
		t.Fatalf("last page: %+v %v", empty, err)
	}
	if _, _, err := r.Conversations(ctx, "", 0); err == nil {
		t.Fatal("unbounded conversation list")
	}
	if _, err := r.SessionImport(ctx, after); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nonimported provenance: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := r.Status(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("status cancel: %v", err)
	}
	if _, _, err := r.Conversations(cancelled, "", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("list cancel: %v", err)
	}
}
