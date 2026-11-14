package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestShellTimeout(t *testing.T) {
	for _, tc := range []struct {
		configured int
		requested  int
		want       time.Duration
	}{
		{0, 0, 0},
		{0, -1, 0},
		{0, 5, 5 * time.Second},
		{10, 0, 10 * time.Second},
		{10, -1, 10 * time.Second},
		{10, 30, 10 * time.Second},
		{10, 5, 5 * time.Second},
		{10, 10, 10 * time.Second},
	} {
		t.Run(fmt.Sprintf("%d/%d", tc.configured, tc.requested), func(t *testing.T) {
			got, err := shellTimeout(tc.configured, tc.requested)
			if err != nil || got != tc.want {
				t.Fatalf("timeout = %v, error = %v, want %v", got, err, tc.want)
			}
		})
	}
	if _, err := shellTimeout(-1, 0); err == nil {
		t.Fatal("accepted negative configured timeout")
	}
	if strconv.IntSize == 64 {
		overflow := maxShellTimeoutSeconds + 1
		if err := ValidateShellTimeout(int(overflow)); err == nil {
			t.Fatal("accepted overflowing configured timeout")
		}
		if _, err := shellTimeout(0, int(overflow)); err == nil {
			t.Fatal("accepted overflowing requested timeout")
		}
		got, err := shellTimeout(10, int(overflow))
		if err != nil || got != 10*time.Second {
			t.Fatal("failed to cap large requested timeout")
		}
	}
}

func TestConfiguredShellTimeout(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZOT_SHELL_HELPER", "1")
	t.Setenv("ZOT_SHELL_HELPER_BLOCK", "1")
	tool := &BashTool{
		CWD:            t.TempDir(),
		Shell:          []string{exe, "-test.run=^TestConfiguredShellHelper$", "--"},
		TimeoutSeconds: 1,
	}
	for _, args := range []string{
		`{"command":"ignored"}`,
		`{"command":"ignored","timeout":0}`,
		`{"command":"ignored","timeout":-1}`,
		`{"command":"ignored","timeout":60}`,
	} {
		t.Run(args, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var progress strings.Builder
			result, err := tool.Execute(ctx, json.RawMessage(args), func(chunk string) {
				progress.WriteString(chunk)
			})
			if err != nil || !result.IsError || result.Status != "timed_out" {
				t.Fatalf("result = %+v, error = %v", result, err)
			}
			if ctx.Err() != nil {
				t.Fatal("caller deadline expired before configured timeout")
			}
			text := result.Content[0].(provider.TextBlock).Text
			if progress.Len() == 0 || !strings.Contains(text, progress.String()) || !strings.Contains(text, "Command timed out.") {
				t.Fatalf("missing partial output or timeout message: %q", text)
			}
		})
	}
}
