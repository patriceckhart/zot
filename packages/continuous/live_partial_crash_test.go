package continuous

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/core"
)

func TestLivePartialProcessCrashRetainsOnlyCheckpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if path := os.Getenv("ZOT_TEST_PARTIAL_CRASH_STORE"); path != "" {
		store, err := openCrashStore(ctx, os.Getenv("ZOT_TEST_PARTIAL_CRASH_BACKEND"), path)
		if err != nil {
			t.Fatal(err)
		}
		r, err := New(store)
		if err != nil {
			t.Fatal(err)
		}
		_, p := livePartialOnRuntime(t, r, time.Hour)
		p.add("saved", core.EvTextDelta{Delta: "saved"})
		p.addReasoning("saved thought", core.EvReasoningDelta{Delta: "saved thought"})
		p.flush()
		p.add(" unsaved", core.EvTextDelta{Delta: " unsaved"})
		p.addReasoning(" unsaved thought", core.EvReasoningDelta{Delta: " unsaved thought"})
		preview, ok, _, err := r.livePartial(ctx, p.base.ConversationID)
		if err != nil || !ok || preview.Text != "saved unsaved" || preview.Reasoning != "saved thought unsaved thought" {
			t.Fatalf("missing unpersisted preview: %+v, %v, %v", preview, ok, err)
		}
		// Deliberately bypass Close, stop, and all deferred cleanup.
		os.Exit(23)
	}
	for _, backend := range crashBackends() {
		t.Run(backend, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store")
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLivePartialProcessCrashRetainsOnlyCheckpoint$")
			cmd.Env = append(os.Environ(), "ZOT_TEST_PARTIAL_CRASH_STORE="+path, "ZOT_TEST_PARTIAL_CRASH_BACKEND="+backend)
			out, err := cmd.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 23 {
				t.Fatalf("child did not reach the crash point: %v\n%s", err, out)
			}
			store, err := openCrashStore(ctx, backend, path)
			if err != nil {
				t.Fatal(err)
			}
			r, err := New(store)
			if err != nil {
				store.Close()
				t.Fatal(err)
			}
			defer r.Close()
			c, err := r.OpenRoot(ctx, "live-preview", AgentConfig{})
			if err != nil {
				t.Fatal(err)
			}
			p, ok, err := r.Partial(ctx, c.ID)
			if err != nil || !ok || p.Text != "saved" || p.Reasoning != "saved thought" || p.PreviewAfter != 0 {
				t.Fatalf("crash checkpoint: %+v, %v, %v", p, ok, err)
			}
			if _, live, _, err := r.livePartial(ctx, c.ID); err != nil || live {
				t.Fatalf("RAM preview survived process exit: %v, %v", live, err)
			}
		})
	}
}
