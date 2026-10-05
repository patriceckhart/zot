package agent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
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

func TestContinuousServeAttach(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "synthetic-key")
	server := &fakeChatServer{script: []string{textChunk("hello from the host")}}
	srv := httptest.NewServer(http.HandlerFunc(server.handler))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "store")
	// Short path: macOS limits Unix socket paths to 104 bytes.
	sockDir, err := os.MkdirTemp("", "zs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sockDir)
	sock := filepath.Join(sockDir, "h.sock")
	tokenFile := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(tokenFile, []byte("# roles\nsubmit-token-0123456789 submit\nread-token-01234567890 read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- runContinuousServe(ctx, []string{"--store", store, "--durability", "process", "--socket", sock, "--token-file", tokenFile, "--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--no-ext", "--no-skills"}, os.Stderr)
	}()
	// Wait for the endpoint (socket file on Unix, named pipe on Windows).
	deadline := time.Now().Add(10 * time.Second)
	for {
		probeCtx, probeCancel := context.WithTimeout(ctx, 200*time.Millisecond)
		conn, err := continuous.DialLocal(probeCtx, sock)
		probeCancel()
		if err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-served:
			t.Fatalf("serve exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("endpoint not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var out bytes.Buffer
	if err := runContinuousAttach(ctx, []string{"say hello", "--socket", sock, "--workspace", "ws", "--token", "submit-token-0123456789"}, &out); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if strings.TrimSpace(out.String()) != "hello from the host" {
		t.Fatalf("answer: %q", out.String())
	}
	// Read-only tokens cannot submit.
	out.Reset()
	err = runContinuousAttach(ctx, []string{"again", "--socket", sock, "--workspace", "ws", "--token", "read-token-01234567890"}, &out)
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("read token submitted: %v", err)
	}
	// Unknown tokens are rejected.
	err = runContinuousAttach(ctx, []string{"again", "--socket", sock, "--workspace", "ws", "--token", "nope-token-0123456789"}, &out)
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("unknown token accepted: %v", err)
	}
	// A world-readable token file is refused by serve.
	if err := os.Chmod(tokenFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTokens(tokenFile); err == nil || !strings.Contains(err.Error(), "accessible") {
		t.Fatalf("permissive token file accepted: %v", err)
	}
	cancel()
	if err := <-served; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("serve: %v", err)
	}
	if runtime.GOOS != "windows" {
		if _, err := os.Stat(sock); err == nil {
			t.Fatal("socket not removed on shutdown")
		}
	}
	failCtx, failCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer failCancel()
	if conn, err := continuous.DialLocal(failCtx, sock); err == nil {
		conn.Close()
		t.Fatal("endpoint still accepting after shutdown")
	}
}

func TestContinuousServeArgs(t *testing.T) {
	if _, err := parseContinuousServeArgs([]string{"--store", "s", "--listen", "0.0.0.0:9000", "--token-file", "t"}); err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("non-loopback listen accepted: %v", err)
	}
	if _, err := parseContinuousServeArgs([]string{"--store", "s", "--listen", "127.0.0.1:9000"}); err == nil || !strings.Contains(err.Error(), "--token-file") {
		t.Fatalf("listen without token accepted: %v", err)
	}
	if _, err := parseContinuousServeArgs([]string{"--store", "s", "--recover", "maybe"}); err == nil {
		t.Fatal("bad recovery policy accepted")
	}
	opts, err := parseContinuousServeArgs([]string{"--store", "s", "--recover", "all", "--model", "m", "--no-ext"})
	if err != nil {
		t.Fatal(err)
	}
	if string(opts.policy) != "all" || strings.Join(opts.hostArgs, " ") != "--model m --no-ext" {
		t.Fatalf("opts: %+v", opts)
	}
	if _, err := parseContinuousAttachArgs([]string{"--socket", "s"}); err == nil {
		t.Fatal("attach without workspace accepted")
	}
	if _, err := parseContinuousAttachArgs([]string{"--socket", "s", "--workspace", "w"}); err == nil {
		t.Fatal("attach without prompt or --follow accepted")
	}
}

func TestContinuousAttachArgsForInteractive(t *testing.T) {
	a, err := ParseArgs([]string{"--continuous", "/tmp/h.sock", "--continuous-workspace", "ws"})
	if err != nil || a.Continuous != "/tmp/h.sock" || a.ContinuousWorkspace != "ws" || !a.NoSess || a.Mode != ModeInteractive {
		t.Fatalf("args: %+v %v", a, err)
	}
	if _, err := ParseArgs([]string{"--continuous", "/tmp/h.sock", "-p", "hi"}); err == nil {
		t.Fatal("print mode with --continuous accepted")
	}
	if _, err := ParseArgs([]string{"--continuous", "/tmp/h.sock", "--continue"}); err == nil {
		t.Fatal("session flag with --continuous accepted")
	}
	if shortAddress("/a/b/host.sock") != "host.sock" || shortAddress("127.0.0.1:9000") != "127.0.0.1:9000" {
		t.Fatal("short address")
	}
}

// A non-loopback listener is refused without TLS. With a certificate, serve
// speaks TLS 1.3 and attach connects with the CA, token still required.
func TestContinuousServeTLS(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "synthetic-key")
	if _, err := parseContinuousServeArgs([]string{"--store", "s", "--listen", "0.0.0.0:9000", "--token-file", "t"}); err == nil || !strings.Contains(err.Error(), "--tls-cert") {
		t.Fatalf("remote listen without TLS accepted: %v", err)
	}
	if _, err := parseContinuousServeArgs([]string{"--store", "s", "--listen", "0.0.0.0:9000", "--token-file", "t", "--tls-cert", "c"}); err == nil {
		t.Fatal("cert without key accepted")
	}
	dir := t.TempDir()
	certFile, keyFile := writeSelfSignedCert(t, dir)
	server := &fakeChatServer{script: []string{textChunk("secure hello")}}
	srv := httptest.NewServer(http.HandlerFunc(server.handler))
	defer srv.Close()
	tokenFile := filepath.Join(dir, "tokens")
	if err := os.WriteFile(tokenFile, []byte("submit-token-0123456789 submit\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	store := filepath.Join(t.TempDir(), "store")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- runContinuousServe(ctx, []string{"--store", store, "--durability", "process", "--listen", addr, "--token-file", tokenFile, "--tls-cert", certFile, "--tls-key", keyFile, "--provider", "openai", "--model", "gpt-4o-mini", "--base-url", srv.URL, "--no-ext", "--no-skills"}, os.Stderr)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-served:
			t.Fatalf("serve exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("listener never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var out bytes.Buffer
	if err := runContinuousAttach(ctx, []string{"say hello", "--address", addr, "--tls-ca", certFile, "--workspace", "ws", "--token", "submit-token-0123456789"}, &out); err != nil {
		t.Fatalf("attach over TLS: %v", err)
	}
	if strings.TrimSpace(out.String()) != "secure hello" {
		t.Fatalf("answer: %q", out.String())
	}
	// A plaintext client cannot talk to the TLS listener.
	if err := runContinuousAttach(ctx, []string{"again", "--address", addr, "--workspace", "ws", "--token", "submit-token-0123456789"}, &out); err == nil {
		t.Fatal("plaintext attach to TLS host succeeded")
	}
	cancel()
	if err := <-served; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("serve: %v", err)
	}
}

func writeSelfSignedCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "zot-test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	keyDER, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}
