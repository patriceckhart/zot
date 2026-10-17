package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestHandleSetupAddGrepGlobal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ZOT_HOME", tmp)

	out, err := handleSetup([]string{"add", "grep"}, tmp)
	if err != nil {
		t.Fatalf("handleSetup add grep: %v", err)
	}
	if out == "" {
		t.Fatal("expected setup output")
	}

	data, err := os.ReadFile(filepath.Join(tmp, "mcp.json"))
	if err != nil {
		t.Fatalf("read mcp.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse mcp.json: %v", err)
	}
	grep := cfg.MCPServers["grep"]
	if grep.Transport != "streamable-http" || grep.URL != "https://mcp.grep.app/" {
		t.Fatalf("unexpected grep config: %+v", grep)
	}
	if len(grep.Headers) != 0 {
		t.Fatalf("protocol headers must not be written to mcp.json: %+v", grep.Headers)
	}
}

func TestHandleSetupAddFilesystemProject(t *testing.T) {
	cwd := t.TempDir()

	_, err := handleSetup([]string{"add", "filesystem", "--project"}, cwd)
	if err != nil {
		t.Fatalf("handleSetup add filesystem: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cwd, ".zot", "mcp.json"))
	if err != nil {
		t.Fatalf("read project mcp.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse project mcp.json: %v", err)
	}
	fs := cfg.MCPServers["filesystem"]
	if fs.Command != "npx" || len(fs.Args) != 3 || fs.Args[2] != cwd {
		t.Fatalf("unexpected filesystem config: %+v", fs)
	}
}

func TestHandleSetupAddYouGlobal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ZOT_HOME", tmp)

	out, err := handleSetup([]string{"add", "you"}, tmp)
	if err != nil {
		t.Fatalf("handleSetup add you: %v", err)
	}
	if out == "" {
		t.Fatal("expected setup output")
	}

	data, err := os.ReadFile(filepath.Join(tmp, "mcp.json"))
	if err != nil {
		t.Fatalf("read mcp.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse mcp.json: %v", err)
	}
	you := cfg.MCPServers["you"]
	if you.Transport != "streamable-http" || you.URL != "https://api.you.com/mcp?profile=free" {
		t.Fatalf("unexpected you config: %+v", you)
	}
	if len(you.Headers) != 0 {
		t.Fatalf("keyless template must not write auth headers: %+v", you.Headers)
	}
}

func TestHandleSetupAddSerplyGlobal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ZOT_HOME", tmp)

	out, err := handleSetup([]string{"add", "serply"}, tmp)
	if err != nil {
		t.Fatalf("handleSetup add serply: %v", err)
	}
	if out == "" {
		t.Fatal("expected setup output")
	}

	data, err := os.ReadFile(filepath.Join(tmp, "mcp.json"))
	if err != nil {
		t.Fatalf("read mcp.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse mcp.json: %v", err)
	}
	serply := cfg.MCPServers["serply"]
	if serply.Transport != "streamable-http" || serply.URL != "https://api.serply.io/mcp" {
		t.Fatalf("unexpected serply config: %+v", serply)
	}
	// The keyed template must reference the key by variable so the written
	// config carries no secret. expandServerEnv resolves it at load time, and
	// drops the server entirely when SERPLY_API_KEY is unset.
	if got := serply.Headers["X-Api-Key"]; got != "${SERPLY_API_KEY}" {
		t.Fatalf("api key must stay an unexpanded variable reference, got %q", got)
	}
}

func TestHandleSetupAddCohesivityGlobal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ZOT_HOME", tmp)

	out, err := handleSetup([]string{"add", "cohesivity"}, tmp)
	if err != nil {
		t.Fatalf("handleSetup add cohesivity: %v", err)
	}
	if out == "" {
		t.Fatal("expected setup output")
	}

	data, err := os.ReadFile(filepath.Join(tmp, "mcp.json"))
	if err != nil {
		t.Fatalf("read mcp.json: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse mcp.json: %v", err)
	}
	coh := cfg.MCPServers["cohesivity"]
	if coh.Transport != "streamable-http" || coh.URL != "https://cohesivity.ai/mcp" {
		t.Fatalf("unexpected cohesivity config: %+v", coh)
	}
	// Keyless template: the only header is the User-Agent; no credential is written.
	if len(coh.Headers) != 1 || coh.Headers["User-Agent"] != "zot-mcp-bridge" {
		t.Fatalf("unexpected cohesivity headers: %+v", coh.Headers)
	}
}

func TestHandleSetupDuplicate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("ZOT_HOME", tmp)

	if _, err := handleSetup([]string{"add", "grep"}, tmp); err != nil {
		t.Fatalf("first add failed: %v", err)
	}
	if _, err := handleSetup([]string{"add", "grep"}, tmp); err == nil {
		t.Fatal("expected duplicate server error")
	}
}
