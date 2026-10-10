package agent

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
)

// webTokenFile is the store-relative file holding the generated browser
// token when serve runs without --token-file.
const webTokenFile = "web-token"

// startContinuousWeb serves the host protocol over WebSocket for browser
// clients (--web). The endpoint always requires a token: --token-file
// tokens when given, otherwise one admin token kept in <store>/web-token
// (mode 0600) so it survives restarts. Only its path is logged. The local
// socket keeps its own authentication either way. It reports the listener's
// exit on errCh after all WebSocket handlers finish.
func startContinuousWeb(ctx context.Context, opts continuousServeOptions, base *continuous.HostServer, tokens map[string]continuous.Role, errCh chan<- error) error {
	web := *base
	tokenPath, token := "", ""
	if len(tokens) == 0 {
		tokenPath = filepath.Join(opts.store, webTokenFile)
		var err error
		token, err = loadOrCreateWebToken(tokenPath)
		if err != nil {
			return err
		}
		web.Tokens = map[string]continuous.Role{token: continuous.RoleAdmin}
	} else {
		web.Tokens = tokens
	}
	webCtx, cancelWeb := context.WithCancel(ctx)
	handler, err := web.WebSocketHandler(webCtx, opts.webOrigins)
	if err != nil {
		cancelWeb()
		return err
	}
	ln, err := net.Listen("tcp", opts.web)
	if err != nil {
		cancelWeb()
		return fmt.Errorf("listen on --web address: %w", err)
	}
	scheme := "ws"
	if opts.tlsCert != "" {
		cfg, err := serverTLSConfig(opts.tlsCert, opts.tlsKey, opts.tlsClientCA)
		if err != nil {
			cancelWeb()
			ln.Close()
			return err
		}
		ln = tls.NewListener(ln, cfg)
		scheme = "wss"
	}
	tracked := &continuousWebHandler{handler: handler}
	srv := &http.Server{Handler: tracked, ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stderr, "  web: %s://%s\n", scheme, ln.Addr())
	if tokenPath != "" {
		fmt.Fprintf(os.Stderr, "  web token: admin token stored in %s\n", tokenPath)
	} else {
		fmt.Fprintln(os.Stderr, "  web token: any token from --token-file")
	}
	go func() {
		<-webCtx.Done()
		srv.Close()
	}()
	go func() {
		err := srv.Serve(ln)
		// HTTP shutdown does not join hijacked WebSocket connections.
		// Cancel them even on listener failure, then join before teardown.
		cancelWeb()
		srv.Close()
		tracked.closeAndWait()
		if errors.Is(err, http.ErrServerClosed) {
			err = context.Canceled
		}
		errCh <- err
	}()
	return nil
}

// continuousWebHandler tracks handlers including hijacked connections.
// The admission lock prevents new WaitGroup additions once shutdown starts.
type continuousWebHandler struct {
	handler http.Handler
	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func (h *continuousWebHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		http.Error(w, "host stopping", http.StatusServiceUnavailable)
		return
	}
	h.wg.Add(1)
	h.mu.Unlock()
	defer h.wg.Done()
	h.handler.ServeHTTP(w, r)
}

func (h *continuousWebHandler) closeAndWait() {
	h.mu.Lock()
	h.closing = true
	h.mu.Unlock()
	h.wg.Wait()
}

// loadOrCreateWebToken reads the browser token or creates a random one.
// An existing file must not be readable by other users on Unix.
func loadOrCreateWebToken(path string) (string, error) {
	if info, err := os.Stat(path); err == nil {
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return "", fmt.Errorf("web token file %s must not be group or world accessible (mode %o)", path, info.Mode().Perm())
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(data))
		if len(token) < 16 {
			return "", fmt.Errorf("web token in %s must be at least 16 characters", path)
		}
		return token, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b[:])
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create web token: %w", err)
	}
	_, werr := f.WriteString(token + "\n")
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("write web token: %w", err)
	}
	return token, nil
}
