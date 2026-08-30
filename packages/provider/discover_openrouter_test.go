package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDiscoverOpenRouterPrefersServedContextLength verifies the
// discovery fix: OpenRouter reports an inflated model-level
// context_length (the model's theoretical max) alongside the serving
// provider's real top_provider.context_length. When the served limit is
// smaller, discovery must use it, because that's the limit OpenRouter
// actually enforces. Using the inflated value made every context-window
// check useless and let max_tokens exceed the real ceiling.
func TestDiscoverOpenRouterPrefersServedContextLength(t *testing.T) {
	const body = `{"data":[
		{"id":"infl","name":"Inflated","context_length":1000000,
		 "top_provider":{"context_length":262144}},
		{"id":"served-bigger","name":"ServedBigger","context_length":131072,
		 "top_provider":{"context_length":262144}},
		{"id":"no-top","name":"NoTop","context_length":128000,
		 "top_provider":{"context_length":0}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	models, err := DiscoverOpenRouter(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Model{}
	for _, m := range models {
		byID[m.ID] = m
	}

	// Inflated model-level window: use the smaller served limit.
	if got := byID["infl"].ContextWindow; got != 262144 {
		t.Errorf("inflated model ContextWindow = %d; want 262144 (served limit)", got)
	}
	// Served limit larger than model-level: keep the smaller model value.
	if got := byID["served-bigger"].ContextWindow; got != 131072 {
		t.Errorf("served-bigger ContextWindow = %d; want 131072 (model value, the smaller one)", got)
	}
	// No served limit: fall back to the model-level value.
	if got := byID["no-top"].ContextWindow; got != 128000 {
		t.Errorf("no-top ContextWindow = %d; want 128000 (model value, no top_provider)", got)
	}
}

func TestDiscoverOpenRouterPresets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/presets" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q; want Bearer test-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"name":"Flash","slug":"flash","status":"active"},
			{"name":"Old","slug":"old","status":"archived"},
			{"name":"Off","slug":"off","status":"disabled"},
			{"name":"NoSlug","slug":"","status":"active"}
		],"total_count":4}`))
	}))
	defer srv.Close()

	models, err := DiscoverOpenRouterPresets(context.Background(), "test-key", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("got %d presets; want 1 active", len(models))
	}
	m := models[0]
	if m.Provider != "openrouter" || m.ID != "@preset/flash" {
		t.Errorf("preset = %+v; want openrouter @preset/flash", m)
	}
	if m.DisplayName != "Flash (preset)" {
		t.Errorf("DisplayName = %q", m.DisplayName)
	}
}
