package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
)

func TestContinuousWebStartupDoesNotLogToken(t *testing.T) {
	logFile, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = logFile
	t.Cleanup(func() {
		os.Stderr = original
		logFile.Close()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := continuousServeOptions{store: t.TempDir(), web: "127.0.0.1:0"}
	done := make(chan error, 1)
	if err := startContinuousWeb(ctx, opts, &continuous.HostServer{}, nil, done); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("web server did not stop")
	}
	token, err := loadOrCreateWebToken(filepath.Join(opts.store, webTokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logFile.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	logs, err := io.ReadAll(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logs), token) {
		t.Fatal("startup logged the admin token")
	}
	if !strings.Contains(string(logs), filepath.Join(opts.store, webTokenFile)) {
		t.Fatal("startup did not log the token path")
	}
}

func TestContinuousWebHandlerJoinsHijackedRequest(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	exited := make(chan struct{})
	h := &continuousWebHandler{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		close(entered)
		<-release
		close(exited)
	})}
	srv := httptest.NewServer(h)
	defer srv.Close()
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, err := http.Get(srv.URL)
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not start")
	}
	joined := make(chan struct{})
	go func() {
		h.closeAndWait()
		close(joined)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		closing := h.closing
		h.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("shutdown did not start")
		}
		runtime.Gosched()
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("request admitted during shutdown: %d", recorder.Code)
	}
	select {
	case <-joined:
		t.Error("shutdown returned before hijacked handler finished")
	default:
	}
	close(release)
	select {
	case <-joined:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join handler")
	}
	<-exited
	<-requestDone
}

func TestContinuousServeWebArgs(t *testing.T) {
	opts, err := parseContinuousServeArgs([]string{"--store", "s", "--web", "127.0.0.1:7787", "--web-origin", "https://app.example"})
	if err != nil {
		t.Fatal(err)
	}
	// --web adds a browser endpoint next to the default local socket.
	if opts.web != "127.0.0.1:7787" || opts.socket == "" || len(opts.webOrigins) != 1 {
		t.Fatalf("unexpected options %+v", opts)
	}
	for name, args := range map[string][]string{
		"remote without TLS": {"--store", "s", "--web", "0.0.0.0:7787"},
		"origin without web": {"--store", "s", "--web-origin", "https://app.example"},
		"missing port":       {"--store", "s", "--web", "localhost"},
		"same as listen":     {"--store", "s", "--web", "127.0.0.1:7787", "--listen", "127.0.0.1:7787", "--token-file", "t"},
	} {
		if _, err := parseContinuousServeArgs(args); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := parseContinuousServeArgs([]string{"--store", "s", "--web", "0.0.0.0:7787"}); err == nil || !strings.Contains(err.Error(), "--tls-cert") {
		t.Fatalf("expected TLS hint, got %v", err)
	}
	if _, err := parseContinuousServeArgs([]string{"--store", "s", "--web", "0.0.0.0:7787", "--tls-cert", "c", "--tls-key", "k"}); err != nil {
		t.Fatalf("remote web with TLS refused: %v", err)
	}
}

func TestWebTokenPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), webTokenFile)
	first, err := loadOrCreateWebToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 32 {
		t.Fatalf("weak token %q", first)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %o", info.Mode().Perm())
	}
	again, err := loadOrCreateWebToken(path)
	if err != nil || again != first {
		t.Fatalf("token changed across restarts: %q %q %v", first, again, err)
	}
}

func TestWebTokenRejectsUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("tiny\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateWebToken(short); err == nil {
		t.Fatal("short token accepted")
	}
	if runtime.GOOS == "windows" {
		return
	}
	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, []byte(strings.Repeat("a", 32)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateWebToken(open); err == nil {
		t.Fatal("world-readable token accepted")
	}
}
