package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/agent/extensions"
)

func TestExtToolAdapterPreservesInteractiveTimeoutPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mock uses /bin/sh; skip on windows")
	}
	root := t.TempDir()
	extDir := filepath.Join(root, "extensions", "adapter-mock")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' '{"type":"hello","name":"adapter-mock","version":"0.1","capabilities":["tools"]}'
printf '%s\n' '{"type":"register_tool","name":"ask","description":"interactive","schema":{"type":"object"},"interactive":true}'
printf '%s\n' '{"type":"register_tool","name":"normal","description":"normal","schema":{"type":"object"}}'
printf '%s\n' '{"type":"ready"}'
while IFS= read -r line; do
  case "$line" in
    *'"type":"shutdown"'*) printf '%s\n' '{"type":"shutdown_ack"}'; exit 0;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(extDir, "run.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(map[string]any{"name": "adapter-mock", "exec": "./run.sh"})
	if err := os.WriteFile(filepath.Join(extDir, "extension.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}

	mgr := extensions.New(root, "", "0.0.0", "openai", "test", nonInteractiveExtHooks{})
	if errs := mgr.Discover(context.Background()); len(errs) != 0 {
		t.Fatalf("discover errs: %v", errs)
	}
	defer mgr.Stop(2 * time.Second)
	mgr.WaitForReady(time.Second)

	adapter := &extToolAdapter{mgr: mgr}
	infos := adapter.Tools()
	if len(infos) != 2 {
		t.Fatalf("got %d tools, want 2: %#v", len(infos), infos)
	}
	for _, info := range infos {
		tool := adapter.NewExtensionTool(info)
		policy, ok := tool.(interface {
			Interactive() bool
			Timeout() time.Duration
		})
		if !ok {
			t.Fatalf("tool %q does not expose timeout policy", info.Name)
		}
		wantInteractive := info.Name == "ask"
		wantTimeout := 60 * time.Second
		if wantInteractive {
			wantTimeout = 0
		}
		if info.Interactive != wantInteractive || policy.Interactive() != wantInteractive || policy.Timeout() != wantTimeout {
			t.Fatalf("tool %q policy = metadata:%v interactive:%v timeout:%s, want interactive:%v timeout:%s", info.Name, info.Interactive, policy.Interactive(), policy.Timeout(), wantInteractive, wantTimeout)
		}
	}
}
