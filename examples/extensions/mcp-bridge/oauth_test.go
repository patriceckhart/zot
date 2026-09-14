package main

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
)

func TestOAuthStore(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	ctx := context.Background()
	s := oauthStoreFor("https://example.test/mcp")
	if _, err := s.GetToken(ctx); !errors.Is(err, transport.ErrNoToken) {
		t.Fatalf("missing token: %v", err)
	}
	if s.path == oauthStoreFor("https://other.test/mcp").path {
		t.Fatal("resource collision")
	}
	if err := s.save(oauthCredentials{ClientID: "synthetic-client"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveToken(ctx, &transport.Token{AccessToken: "synthetic-token", RefreshToken: "synthetic-refresh"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.read()
	if err != nil || c.ClientID != "synthetic-client" || c.Token.RefreshToken != "synthetic-refresh" {
		t.Fatal("credentials did not round trip")
	}
	info, err := os.Stat(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatal("insecure token permissions")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetToken(canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("read ignored cancellation")
	}
	if err := s.SaveToken(canceled, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("write ignored cancellation")
	}
	if err := os.WriteFile(s.path, []byte("synthetic-secret-invalid-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(); err == nil || err.Error() != "invalid OAuth credential file" {
		t.Fatal("unsafe corruption diagnostic")
	}
}

func TestOAuthLoginRejectsInsecureResource(t *testing.T) {
	s := &managedServer{config: ServerConfig{Transport: "streamable-http", URL: "http://example.test/mcp"}}
	if err := s.login(context.Background(), func(string) { t.Fatal("unexpected authorization") }); err == nil {
		t.Fatal("accepted insecure resource")
	}
}
