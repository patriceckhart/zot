package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestDiscoverXAIModelsAndAliases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Errorf("unexpected discovery request: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"data":[
			{"id":"grok-4.8","aliases":["grok-latest","grok-4.7","grok-latest",""],"context_length":700000,
			 "prompt_text_token_price":20000,"completion_text_token_price":60000,"cached_prompt_text_token_price":5000,
			 "prompt_image_token_price":20000,"long_context_threshold":200000,
			 "prompt_text_token_price_long_context":40000,"completion_text_token_price_long_context":120000,
			 "cached_prompt_text_token_price_long_context":0,"capabilities":{"reasoning_effort":["low","high"]}},
			{"id":"grok-4.7"}, {"id":"grok-4.8"}, {"id":"grok-unpublished"},
			{"id":"grok-imagine-image","image_price":200000000}, {"id":"grok-video"},
			{"id":"grok-voice"}, {"id":"grok-embed"}, {"id":"grok-audio"},
			{"id":"grok-media","output_modalities":["video"]}, {"id":"grok-generation","image_price":1},
			{"id":""}, {"id":"unrelated"}
		]}`)
	}))
	defer server.Close()

	models, err := DiscoverXAI(context.Background(), "synthetic-key", server.URL+"/v1/")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, model := range models {
		ids = append(ids, model.ID)
		if model.Provider != "xai" || model.API != APIResponses || model.MaxOutput != 0 || model.BaseURL != server.URL+"/v1" || model.Source != "live" {
			t.Fatalf("model = %+v", model)
		}
	}
	if !slices.Equal(ids, []string{"grok-4.8", "grok-latest", "grok-4.7", "grok-unpublished"}) {
		t.Fatalf("IDs = %v", ids)
	}
	m := models[0]
	if m.ContextWindow != 700000 || !m.Reasoning || !slices.Equal(m.Input, []string{"text", "image"}) ||
		m.PriceInput != 2 || m.PriceOutput != 6 || m.PriceCacheRead != 0.5 ||
		m.PriceTierInputTokens != 199999 || m.PriceInputAbove != 4 || m.PriceOutputAbove != 12 || m.PriceCacheReadAbove != 0.5 {
		t.Fatalf("live metadata = %+v", m)
	}
	if got := AvailableReasoningLevels(m); !slices.Equal(got, []string{"", "low", "high"}) {
		t.Fatalf("reasoning levels = %v", got)
	}
	for _, prompt := range []int{199999, 200000} {
		u := Usage{InputTokens: prompt - 1000, CacheReadTokens: 1000, OutputTokens: 10}
		input, output := 2.0, 6.0
		if prompt == 200000 {
			input, output = 4, 12
		}
		want := (float64(u.InputTokens)*input + 1000*0.5 + 10*output) / 1e6
		if got := ComputeCost(m, u); math.Abs(got-want) > 1e-12 {
			t.Fatalf("prompt %d: cost=%v, want=%v", prompt, got, want)
		}
	}
	// A real primary record takes precedence over another model's alias.
	known, _ := FindModel("xai", "grok-4.7")
	if models[2].ContextWindow != known.ContextWindow || models[2].PriceInputAbove != known.PriceInputAbove {
		t.Fatalf("ID-only response lost known metadata: %+v", models[2])
	}
	unknown := models[3]
	if unknown.ContextWindow != 32768 || unknown.Reasoning || unknown.PriceInput != 0 {
		t.Fatalf("unknown metadata was invented: %+v", unknown)
	}
	// Refresh data must override known metadata, not just add unknown IDs.
	merged := MergeCatalog([]Model{m, {Provider: "xai", ID: "grok-4.7", ContextWindow: 700000, Source: "live"}})
	for _, model := range merged {
		if model.Provider == "xai" && model.ID == "grok-4.7" && model.ContextWindow != 700000 {
			t.Fatalf("catalog suppressed live metadata: %+v", model)
		}
	}
}

func TestDiscoverXAIPreservesCatalogAndZeroPrices(t *testing.T) {
	before := slices.Clone(Catalog)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"grok-4.7","context_length":800000,"prompt_text_token_price":0,
			"completion_text_token_price":0,"cached_prompt_text_token_price":0,"long_context_threshold":0,
			"input_modalities":["text"],"capabilities":{"reasoning_effort":["none"]}}]}`)
	}))
	defer server.Close()
	models, err := DiscoverXAI(context.Background(), "synthetic-key", server.URL)
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%v, err=%v", models, err)
	}
	m := models[0]
	if m.ContextWindow != 800000 || m.PriceInput != 0 || m.PriceOutput != 0 || m.PriceCacheRead != 0 || m.PriceTierInputTokens != 0 || m.Reasoning {
		t.Fatalf("explicit zero metadata was ignored: %+v", m)
	}
	if !reflect.DeepEqual(before, Catalog) {
		t.Fatal("discovery mutated the built-in catalog")
	}
}

func TestDiscoverXAIErrorsAndEmptyList(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"unauthorized", "private-token", 401, true},
		{"server error", "private-token", 503, true},
		{"invalid JSON", "private-token", 200, true},
		{"missing data", `{}`, 200, true},
		{"null data", `{"data":null}`, 200, true},
		{"bad data", `{"data":[{"id":"grok-x","context_length":"private-token"}]}`, 200, true},
		{"oversized", `{"data":[],"padding":"` + strings.Repeat("x", 8<<20) + `"}`, 200, true},
		{"empty", `{"data":[]}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			models, err := DiscoverXAI(context.Background(), "synthetic-key", server.URL)
			if (err != nil) != tc.wantError || len(models) != 0 {
				t.Fatalf("models=%v, err=%v", models, err)
			}
			if err != nil && strings.Contains(err.Error(), "private-token") {
				t.Fatal("response payload leaked into error")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverXAI(ctx, "synthetic-key", "http://127.0.0.1:1/v1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: %v", err)
	}
	if _, err := DiscoverXAI(context.Background(), "synthetic-key", "not-an-endpoint"); err == nil {
		t.Fatal("invalid base URL accepted")
	}
}

func TestXAIUnlistedResponsesRouting(t *testing.T) {
	completions := &routeCaptureClient{name: "xai"}
	responses := &routeCaptureClient{name: "xai"}
	router := NewModelRouter("xai", completions, map[string]Client{APIResponses: responses})
	for _, id := range []string{"grok-future-unlisted", "grok-build-0.1"} {
		stream, err := router.Stream(context.Background(), Request{Model: id})
		if err != nil {
			t.Fatal(err)
		}
		for range stream {
		}
	}
	if !slices.Equal(completions.models, []string{"grok-build-0.1"}) || !slices.Equal(responses.models, []string{"grok-future-unlisted"}) {
		t.Fatalf("Completions=%v, Responses=%v", completions.models, responses.models)
	}
	client := &codexClient{providerName: "xai"}
	wire, err := client.buildRequest(Request{Model: "grok-future-unlisted"})
	if err != nil || wire.Model != "grok-future-unlisted" {
		t.Fatalf("unlisted Responses request=%+v, err=%v", wire, err)
	}
}
