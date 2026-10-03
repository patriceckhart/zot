package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/agent/codemode"
)

func TestCodemodeActivationAndPresentation(t *testing.T) {
	budget := 0
	cfg := Config{Codemode: &codemode.Settings{Enabled: true, Mode: "only", InlineBudget: &budget}}
	for _, goos := range []string{"darwin", "linux", "windows"} {
		reg := buildConfiguredToolRegistry(Args{}, t.TempDir(), nil, cfg, goos)
		if reg["read"] == nil || reg["codemode"] == nil {
			t.Fatal("configured tools missing")
		}
		specs := reg.Specs()
		if len(specs) != 1 || specs[0].Name != "codemode" {
			t.Fatalf("only mode: %+v", specs)
		}
		reg = buildConfiguredToolRegistry(Args{CodemodeMode: "on", Codemode: true}, t.TempDir(), nil, cfg, goos)
		if len(reg.Specs()) != len(reg) {
			t.Fatal("CLI mode did not override settings")
		}
	}
	reg := buildConfiguredToolRegistry(Args{NoTools: true, Codemode: true}, t.TempDir(), nil, cfg, "darwin")
	if len(reg) != 0 {
		t.Fatal("no-tools lost precedence")
	}
	reg = buildConfiguredToolRegistry(Args{Tools: []string{"read"}}, t.TempDir(), nil, cfg, "darwin")
	if reg["codemode"] != nil {
		t.Fatal("saved preference expanded explicit tool selection")
	}
	for _, settings := range []*codemode.Settings{{Mode: "invalid"}, {InlineBudget: func() *int { n := -1; return &n }()}} {
		if settings.Validate() == nil {
			t.Fatalf("accepted %+v", settings)
		}
	}
}

func TestCodemodeModelEndpointAndCredentialIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-host-key" {
			t.Error("host credential not used")
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1}}`)
	}))
	defer server.Close()
	caller := newCodemodeModelCaller("openrouter", "synthetic-host-key", server.URL)
	raw := json.RawMessage(`[{"id":"google/gemini-2.5-flash-image","provider":"openrouter","baseUrl":"https://invalid.example","apiKey":"script-key"},{"input":[{"type":"text","text":"drawing"}]}]`)
	result, usage, err := caller(context.Background(), "generateImages", raw)
	if err != nil || usage == nil || strings.Contains(string(result), "synthetic-host-key") || !strings.Contains(string(result), `"stopReason":"stop"`) {
		t.Fatalf("result=%s usage=%+v err=%v", result, usage, err)
	}
	result, _, err = caller(context.Background(), "getModelsOfType", json.RawMessage(`["image","openrouter"]`))
	if err != nil || strings.Contains(string(result), "baseUrl") || strings.Contains(string(result), "synthetic-host-key") {
		t.Fatalf("catalog exposed host data: %s %v", result, err)
	}
	if _, _, err = caller(context.Background(), "classify", json.RawMessage(`[{"id":"unknown","provider":"openrouter"},{}]`)); err == nil {
		t.Fatal("unknown model executed")
	}
	result, _, err = caller(context.Background(), "getModelOfType", json.RawMessage(`["image","openrouter","unknown"]`))
	if err != nil || result != nil {
		t.Fatalf("unknown model: %s %v", result, err)
	}
}

func TestTypeSafeEnvironmentCredential(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "synthetic")
	key, method, err := ResolveCredentialContext(context.Background(), "typesafe", "")
	if err != nil || key != "synthetic" || method != "apikey" || !CredentialAvailable("typesafe") {
		t.Fatalf("credential resolution: method=%s err=%v", method, err)
	}
}
