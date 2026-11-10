package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoginAPIKeyWithoutBrowser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	m := NewManager(NewStore(path))
	t.Cleanup(m.Close)
	m.probeKey = func(ctx context.Context, provider, key string) error {
		if provider != "anthropic" || key != "synthetic-key" {
			t.Fatal("unexpected probe input")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("probe has no deadline")
		}
		return nil
	}
	if err := m.LoginAPIKey(context.Background(), "anthropic", "  synthetic-key\n"); err != nil {
		t.Fatal(err)
	}
	if m.keyServer != nil || m.oauthServer != nil {
		t.Fatal("terminal login started a listener")
	}
	creds, err := m.Store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if creds.Anthropic.APIKey != "synthetic-key" {
		t.Fatal("key not stored")
	}
	if ev := <-m.Events(); ev.Kind != "success" || ev.Provider != "anthropic" || ev.Method != "apikey" || ev.URL != "" {
		t.Fatalf("event = %+v", ev)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("credential mode = %o", info.Mode().Perm())
		}
	}
}

func TestLoginAPIKeyRejectsInvalidInput(t *testing.T) {
	for _, tc := range []struct {
		name, provider, key string
	}{
		{"empty", "anthropic", " \n"},
		{"multiline", "anthropic", "synthetic-key\nsecond-key"},
		{"NUL", "anthropic", "synthetic-key\x00"},
		{"oversize", "anthropic", strings.Repeat("x", 64*1024+1)},
		{"unknown provider", "not-a-provider", "synthetic-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(NewStore(filepath.Join(t.TempDir(), "auth.json")))
			t.Cleanup(m.Close)
			m.probeKey = func(context.Context, string, string) error {
				t.Fatal("invalid input was probed")
				return nil
			}
			if err := m.LoginAPIKey(context.Background(), tc.provider, tc.key); err == nil {
				t.Fatal("invalid input accepted")
			}
			if ev := <-m.Events(); ev.Kind != "error" || strings.Contains(ev.Message, "synthetic-key") {
				t.Fatalf("event = %+v", ev)
			}
		})
	}
}

func TestLoginAPIKeyProbeFailurePreservesCredential(t *testing.T) {
	m := NewManager(NewStore(filepath.Join(t.TempDir(), "auth.json")))
	t.Cleanup(m.Close)
	if err := m.Store().SetAPIKey("anthropic", "previous-synthetic-key"); err != nil {
		t.Fatal(err)
	}
	m.probeKey = func(context.Context, string, string) error {
		return errors.New("provider rejected the key")
	}
	if err := m.LoginAPIKey(context.Background(), "anthropic", "new-synthetic-key"); err == nil {
		t.Fatal("probe failure accepted")
	}
	creds, err := m.Store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if creds.Anthropic.APIKey != "previous-synthetic-key" {
		t.Fatal("failed probe replaced the existing credential")
	}
	if ev := <-m.Events(); ev.Kind != "error" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestLoginAPIKeyCancellationDoesNotSaveOrEmit(t *testing.T) {
	m := NewManager(NewStore(filepath.Join(t.TempDir(), "auth.json")))
	t.Cleanup(m.Close)
	started := make(chan struct{})
	m.probeKey = func(ctx context.Context, _, _ string) error {
		close(started)
		<-ctx.Done()
		return nil // Even an uncooperative probe must not save after cancellation.
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- m.LoginAPIKey(ctx, "anthropic", "synthetic-key") }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled login did not return")
	}
	creds, err := m.Store().Load()
	if err != nil {
		t.Fatal(err)
	}
	if creds.Has("anthropic") {
		t.Fatal("canceled key was saved")
	}
	select {
	case ev := <-m.Events():
		t.Fatalf("canceled login emitted %+v", ev)
	default:
	}
}

func TestLoginAPIKeyCanceledBeforeProbe(t *testing.T) {
	m := NewManager(NewStore(filepath.Join(t.TempDir(), "auth.json")))
	t.Cleanup(m.Close)
	m.probeKey = func(context.Context, string, string) error {
		t.Fatal("canceled login probed key")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.LoginAPIKey(ctx, "anthropic", "synthetic-key"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
