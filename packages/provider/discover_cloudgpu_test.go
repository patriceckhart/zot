package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestDiscoverCloudGPUMapsChatModels(t *testing.T) {
	const body = `{"object":"list","data":[
		{"id":"glm-5.2","object":"model","owned_by":"cloudgpu","input_usd_per_1m":"1.26","output_usd_per_1m":"3.96","cached_input_usd_per_1m":"0.23"},
		{"id":"kimi-k2.6","name":"Kimi K2.6","object":"model","owned_by":"cloudgpu","input_usd_per_1m":"0.855","output_usd_per_1m":"3.6","cached_input_usd_per_1m":"0.4275"},
		{"id":"minimax-m3","object":"model","owned_by":"cloudgpu","input_usd_per_1m":"bad","output_usd_per_1m":"-1","cached_input_usd_per_1m":""},
		{"id":"glm-5.2","object":"model","owned_by":"cloudgpu"},
		{"id":"auto","object":"model","owned_by":"cloudgpu"},
		{"id":"flux-2-pro","object":"model","owned_by":"cloudgpu"},
		{"id":"kokoro-tts","object":"model","owned_by":"cloudgpu"},
		{"id":"whisper-v3","object":"model","owned_by":"cloudgpu"},
		{"id":"sample-asr","object":"model","owned_by":"cloudgpu"},
		{"id":"","object":"model","owned_by":"cloudgpu"},
		{"id":"  ","object":"model","owned_by":"cloudgpu"}
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

	models, err := DiscoverCloudGPU(context.Background(), srv.URL+"/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %+v", models)
	}

	glm := models[0]
	if glm.Provider != "cloudgpu" || glm.ID != "glm-5.2" || glm.DisplayName != "glm-5.2" ||
		glm.ContextWindow != customDiscoveryContextWindow || glm.MaxOutput != customDiscoveryMaxOutput || glm.Reasoning ||
		glm.PriceInput != 1.26 || glm.PriceOutput != 3.96 || glm.PriceCacheRead != 0.23 ||
		glm.BaseURL != srv.URL+"/v1" || glm.Source != "live" || !slices.Equal(glm.Input, []string{"text"}) {
		t.Errorf("glm model = %+v", glm)
	}
	kimi := models[1]
	if kimi.ID != "kimi-k2.6" || kimi.DisplayName != "Kimi K2.6" || kimi.PriceInput != 0.855 || kimi.PriceOutput != 3.6 || kimi.PriceCacheRead != 0.4275 {
		t.Errorf("kimi model = %+v", kimi)
	}
	fallback := models[2]
	if fallback.ID != "minimax-m3" || fallback.PriceInput != 0 || fallback.PriceOutput != 0 || fallback.PriceCacheRead != 0 {
		t.Errorf("fallback metadata = %+v", fallback)
	}

	SetLiveModels(models)
	t.Cleanup(func() { SetLiveModels(nil) })
	client := NewCloudGPU("test-key", srv.URL+"/v1").(*openaiClient)
	wire, err := client.buildRequest(Request{Model: kimi.ID})
	if err != nil {
		t.Fatal(err)
	}
	if wire.Model != kimi.ID || wire.MaxTokens == nil || *wire.MaxTokens != customDiscoveryMaxOutput || wire.MaxCompletionTok != nil || wire.ReasoningEffort != "" {
		t.Errorf("wire request = %+v", wire)
	}
}

func TestDiscoverCloudGPUReplacesSeed(t *testing.T) {
	SetLiveModels([]Model{{
		Provider: "cloudgpu", ID: "deepseek-v4-flash", DisplayName: "DeepSeek V4 Flash",
		ContextWindow: 524288, MaxOutput: 65536, PriceInput: 0.2, Source: "live",
	}})
	t.Cleanup(func() { SetLiveModels(nil) })
	m, err := FindModel("cloudgpu", "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	if m.ContextWindow != 524288 || m.MaxOutput != 65536 || m.PriceInput != 0.2 {
		t.Errorf("seed metadata was not replaced: %+v", m)
	}
}

func TestDiscoverCloudGPUErrors(t *testing.T) {
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
			if _, err := DiscoverCloudGPU(context.Background(), srv.URL); err == nil {
				t.Fatal("expected discovery error")
			}
		})
	}
}
