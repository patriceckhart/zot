package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage/memory"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestContinuousProviderSwitchKeepsHistoryAndRoutesRequests(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type request struct {
		provider string
		keyOK    bool
		messages int
	}
	requests := make(chan request, 4)
	providers := map[string]provider.UserProvider{}
	for _, name := range []string{"routing-first", "routing-second"} {
		name := name
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Messages []json.RawMessage `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			requests <- request{name, r.Header.Get("Authorization") == "Bearer synthetic-"+name, len(body.Messages)}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"answer"},"finish_reason":null}]}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, "data: [DONE]")
			fmt.Fprintln(w)
		}))
		t.Cleanup(server.Close)
		providers[name] = provider.UserProvider{
			API: "openai", BaseURL: server.URL,
			Models: []provider.UserModel{{ID: "shared", MaxTokens: 1000, ContextWindow: 10000}},
		}
		if err := AuthStoreFor().SetAPIKey(name, "synthetic-"+name); err != nil {
			t.Fatal(err)
		}
	}
	modelsJSON, err := json.Marshal(provider.UserModelsFile{Providers: providers})
	if err != nil {
		t.Fatal(err)
	}
	loadTestModelsJSON(t, string(modelsJSON))
	rt, err := continuous.New(memory.Open())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	host := Resolved{Provider: "routing-first", Model: "shared", Credential: "synthetic-routing-first", BaseURL: provider.CustomProviders()["routing-first"].BaseURL}
	var service *continuous.Service
	engine := newContinuousEngine(host, nil, rt, func() *continuous.Service { return service })
	service, err = continuous.NewService(rt, engine, continuous.ExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := rt.OpenRoot(ctx, "switch", continuous.AgentConfig{Provider: host.Provider, Model: host.Model})
	if err != nil {
		t.Fatal(err)
	}
	for turn, name := range []string{"routing-first", "routing-second"} {
		if turn > 0 {
			current, err := rt.OpenRoot(ctx, "switch", continuous.AgentConfig{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rt.Configure(ctx, c.ID, current.Revision, continuous.AgentConfig{Provider: name, Model: "shared"}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := rt.Submit(ctx, c.ID, "actor", "", "question"); err != nil {
			t.Fatal(err)
		}
		if run, _, err := service.Step(ctx, c.ID); err != nil || run.Outcome != "completed" {
			t.Fatalf("turn %d: %+v %v", turn, run, err)
		}
		select {
		case got := <-requests:
			if got.provider != name || !got.keyOK || got.messages != 1+2*turn {
				t.Fatalf("incorrect request routing or history: provider=%s keyOK=%t messages=%d", got.provider, got.keyOK, got.messages)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if report, err := rt.CheckIntegrity(ctx); err != nil || !report.Valid {
		t.Fatalf("invalid transcript: %+v %v", report, err)
	}
}
