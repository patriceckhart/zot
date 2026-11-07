package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestRPCSetModelRejectsDisabledProvider(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{DisabledProviders: []string{"anthropic"}}); err != nil {
		t.Fatal(err)
	}
	const from, to = "claude-3-5-haiku-20241022", "claude-opus-4-1-20250805"
	ag := &core.Agent{Model: from, MaxTokens: 4096}
	var out bytes.Buffer
	s := &rpcServer{ctx: context.Background(), agent: ag, provider: "anthropic", model: from, out: &out}
	raw, err := json.Marshal(map[string]string{"model": to})
	if err != nil {
		t.Fatal(err)
	}
	s.dispatch("set_model", "1", raw)
	s.inFlight.Wait()
	if ag.Model != from || ag.MaxTokens != 4096 || !strings.Contains(out.String(), "disabled_providers") {
		t.Fatalf("disabled set_model: model = %q, max = %d, response = %s", ag.Model, ag.MaxTokens, &out)
	}
}

func TestRPCSetModelUpdatesMaxTokens(t *testing.T) {
	const to = "claude-opus-4-1-20250805"
	want, err := provider.FindModel("anthropic", to)
	if err != nil {
		t.Fatal(err)
	}
	ag := core.NewAgent(&startCaptureClient{}, "claude-3-5-haiku-20241022", "", nil)
	ag.MaxTokens = 4096
	var out bytes.Buffer
	s := &rpcServer{ctx: context.Background(), agent: ag, provider: "anthropic", model: ag.Model, out: &out}

	raw, err := json.Marshal(map[string]string{"model": to})
	if err != nil {
		t.Fatal(err)
	}
	s.dispatch("set_model", "1", raw)
	s.inFlight.Wait()

	if ag.Model != to || ag.MaxTokens != want.MaxOutput {
		t.Fatalf("after set_model: model=%q MaxTokens=%d, want %q %d (out=%s)", ag.Model, ag.MaxTokens, to, want.MaxOutput, out.String())
	}
}
