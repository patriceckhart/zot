package provider

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestDiscoverUberLLMMapsChatModels(t *testing.T) {
	const body = `{"object":"list","data":[
		{"id":"z-ai/glm-5.3","display_name":"GLM 5.3","type":"chat","available":true,"input_modalities":["text"],"output_modalities":["text"],
		 "context_length":200000,"max_output_tokens":131072,"supports_tools":true,"supports_reasoning":true,
		 "pricing":{"prompt":"0.0000005","completion":"0.0000022","input_usd":0.5,"output_usd":2.2,"unit":"1M tokens"}},
		{"id":"moonshotai/kimi-k2.6","display_name":"Kimi K2.6","type":"chat","available":true,"input_modalities":["text","image"],"output_modalities":["text"],
		 "context_length":262144,"max_output_tokens":16384,"supports_tools":true,"supports_reasoning":true,
		 "pricing":{"input_usd":0.6,"output_usd":2.5}},
		{"id":"vendor/no-limits","type":"chat","input_modalities":["text"],"output_modalities":["text"],"supports_tools":true},
		{"id":"z-ai/glm-5.3","display_name":"Duplicate","type":"chat","input_modalities":["text"],"output_modalities":["text"],"supports_tools":true},
		{"id":"vendor/no-tools","type":"chat","input_modalities":["text"],"output_modalities":["text"],"supports_tools":false},
		{"id":"vendor/unavailable","type":"chat","available":false,"input_modalities":["text"],"output_modalities":["text"],"supports_tools":true},
		{"id":"vendor/image-model","type":"image","input_modalities":["text"],"output_modalities":["image"],"supports_tools":true},
		{"id":"vendor/router","type":"router","input_modalities":["text"],"output_modalities":["decisions"],"supports_tools":true},
		{"id":"vendor/speech-to-text","type":"chat","input_modalities":["audio"],"output_modalities":["text"],"supports_tools":true},
		{"id":"","type":"chat","input_modalities":["text"],"output_modalities":["text"],"supports_tools":true}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("request path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("public catalog request carried a credential")
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	models, err := DiscoverUberLLM(context.Background(), srv.URL+"/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}

	glm := models[0]
	if glm.Provider != "uberllm" || glm.ID != "z-ai/glm-5.3" || glm.DisplayName != "GLM 5.3" ||
		glm.ContextWindow != 200000 || glm.MaxOutput != 131072 || !glm.Reasoning ||
		glm.PriceInput != 0.5 || glm.PriceOutput != 2.2 ||
		glm.BaseURL != srv.URL+"/v1" || glm.Source != "live" || !slices.Equal(glm.Input, []string{"text"}) {
		t.Errorf("glm model = %+v", glm)
	}
	kimi := models[1]
	if kimi.ID != "moonshotai/kimi-k2.6" || kimi.MaxOutput != 16384 || !slices.Equal(kimi.Input, []string{"text", "image"}) {
		t.Errorf("kimi model = %+v", kimi)
	}
	fallback := models[2]
	if fallback.DisplayName != "vendor/no-limits" || fallback.ContextWindow != 32768 || fallback.MaxOutput != 4096 || fallback.Reasoning {
		t.Errorf("fallback metadata = %+v", fallback)
	}

	SetLiveModels(models)
	t.Cleanup(func() { SetLiveModels(nil) })
	client := NewUberLLM("test-key", srv.URL+"/v1").(*openaiClient)
	wire, err := client.buildRequest(Request{Model: kimi.ID, Reasoning: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Model != kimi.ID || wire.MaxCompletionTok == nil || *wire.MaxCompletionTok != 16384 || wire.ReasoningEffort != "high" {
		t.Errorf("wire request = %+v", wire)
	}
}

func TestDiscoverUberLLMCachePricing(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pricing string
		read    float64
		write   float64
	}{
		{"published", `"cache_read_usd":0.1,"cache_write_usd":1.25`, 0.1, 1.25},
		{"missing", `"input_usd":1`, 0, 0},
		{"negative", `"cache_read_usd":-0.1,"cache_write_usd":-1.25`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"data":[{"id":"vendor/model","input_modalities":["text"],"output_modalities":["text"],"supports_tools":true,"pricing":{%s}}]}`, tc.pricing)
			}))
			defer srv.Close()
			models, err := DiscoverUberLLM(context.Background(), srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if len(models) != 1 {
				t.Fatalf("models = %+v, want one model", models)
			}
			m := models[0]
			if m.PriceCacheRead != tc.read || m.PriceCacheWrite != tc.write {
				t.Errorf("cache prices = %g/%g, want %g/%g", m.PriceCacheRead, m.PriceCacheWrite, tc.read, tc.write)
			}
			got := ComputeCost(m, Usage{CacheReadTokens: 1000000, CacheWriteTokens: 2000000})
			want := tc.read + 2*tc.write
			if math.Abs(got-want) > 1e-12 {
				t.Errorf("cached token cost = %g, want %g", got, want)
			}
		})
	}
}

func TestDiscoverUberLLMReplacesSeed(t *testing.T) {
	SetLiveModels([]Model{{
		Provider: "uberllm", ID: "deepseek/deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash",
		ContextWindow: 524288, MaxOutput: 65536, PriceInput: 0.2, Source: "live",
	}})
	t.Cleanup(func() { SetLiveModels(nil) })
	m, err := FindModel("uberllm", "deepseek/deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	if m.ContextWindow != 524288 || m.MaxOutput != 65536 || m.PriceInput != 0.2 {
		t.Errorf("seed metadata was not replaced: %+v", m)
	}
}

func TestDiscoverUberLLMErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unavailable", http.StatusServiceUnavailable, "unavailable"},
		{"invalid-json", http.StatusOK, "not json"},
		{"missing-data", http.StatusOK, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer srv.Close()
			if _, err := DiscoverUberLLM(context.Background(), srv.URL); err == nil {
				t.Fatal("expected discovery error")
			}
		})
	}
}
