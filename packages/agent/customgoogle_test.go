package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
)

func TestCustomGoogleProvider(t *testing.T) {
	for _, noAuth := range []bool{false, true} {
		t.Run(fmt.Sprintf("keyless=%v", noAuth), func(t *testing.T) {
			t.Setenv("ZOT_HOME", t.TempDir())
			t.Setenv("MY_GEMINI_API_KEY", "custom-key")
			t.Setenv("GOOGLE_API_KEY", "builtin-key")
			builtin, err := provider.FindModel("google", "gemini-2.5-pro")
			if err != nil {
				t.Fatal(err)
			}
			var listCalls, streamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantKey := "custom-key"
				if noAuth {
					wantKey = ""
				}
				if got := r.Header.Get("x-goog-api-key"); got != wantKey {
					t.Errorf("key = %q, want %q", got, wantKey)
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("unexpected bearer authorization")
				}
				switch r.URL.Path {
				case "/v1beta/models":
					listCall := listCalls.Add(1)
					if r.Method != http.MethodGet {
						t.Errorf("discovery method = %s", r.Method)
					}
					if listCall == 1 {
						_, _ = io.WriteString(w, `{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"],"inputTokenLimit":90000,"outputTokenLimit":6000},{"name":"models/embedding","supportedGenerationMethods":["embedContent"]}],"nextPageToken":"a+b &c"}`)
					} else {
						if r.URL.Query().Get("pageToken") != "a+b &c" {
							t.Errorf("pageToken = %q", r.URL.Query().Get("pageToken"))
						}
						_, _ = io.WriteString(w, `{"models":[{"name":"models/proxy-chat","supportedGenerationMethods":["streamGenerateContent"],"inputTokenLimit":32000,"outputTokenLimit":2000},{"name":"models/gemini-3-flash","supportedGenerationMethods":["generateContent"],"inputTokenLimit":100000,"outputTokenLimit":8000}]}`)
					}
				case "/v1beta/models/gemini-2.5-pro:streamGenerateContent":
					streamCalls.Add(1)
					if r.Method != http.MethodPost || r.URL.Query().Get("alt") != "sse" {
						t.Errorf("stream request = %s %s", r.Method, r.URL)
					}
					var body struct {
						GenerationConfig struct {
							MaxOutputTokens int `json:"maxOutputTokens"`
							ThinkingConfig  struct {
								ThinkingBudget int `json:"thinkingBudget"`
							} `json:"thinkingConfig"`
						} `json:"generationConfig"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body.GenerationConfig.MaxOutputTokens != 1234 || body.GenerationConfig.ThinkingConfig.ThinkingBudget != provider.ReasoningBudget("high") {
						t.Errorf("custom generation config ignored: %+v", body.GenerationConfig)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"candidates":[{"content":{"role":"model","parts":[{"text":"thought","thought":true},{"text":"hello"}]},"finishReason":"STOP"}]}`+"\n\n")
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			auth := ""
			if noAuth {
				auth = `,"auth":"none"`
			}
			loadTestModelsJSON(t, fmt.Sprintf(`{"providers":{"my-gemini":{"api":"google","baseUrl":%q,"discover":true%s,"models":[{"id":"gemini-2.5-pro","reasoning":true,"maxTokens":1234,"reasoningLevelMap":{"low":"high"}}]}}}`, server.URL, auth))
			if cfg := provider.CustomProviders()["my-gemini"]; cfg.API != "google" {
				t.Fatalf("API = %q", cfg.API)
			}
			if err := RefreshCustomProviderModels(context.Background()); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"proxy-chat", "gemini-3-flash"} {
				m, err := provider.FindModel("my-gemini", id)
				if err != nil {
					t.Fatal(err)
				}
				if m.Provider != "my-gemini" || m.API != "google" || m.BaseURL != server.URL || m.ContextWindow <= 0 || m.MaxOutput <= 0 {
					t.Fatalf("discovered model = %+v", m)
				}
				if id == "gemini-3-flash" && !m.Reasoning {
					t.Error("discovered Gemini model lost reasoning support")
				}
			}
			if _, err := provider.FindModel("my-gemini", "embedding"); err == nil {
				t.Error("embedding discovered as chat model")
			}
			resolved, err := Resolve(Args{Provider: "my-gemini", Model: "gemini-2.5-pro"}, false)
			if err != nil {
				t.Fatal(err)
			}
			client := resolved.NewClient()
			if client.Name() != "my-gemini" {
				t.Fatalf("client name = %q", client.Name())
			}
			events, err := client.Stream(context.Background(), provider.Request{Model: "gemini-2.5-pro", Reasoning: "low"})
			if err != nil {
				t.Fatal(err)
			}
			var started, thought, text bool
			for event := range events {
				switch e := event.(type) {
				case provider.EventStart:
					started = e.Provider == "my-gemini"
				case provider.EventReasoningDelta:
					thought = e.Delta == "thought"
				case provider.EventTextDelta:
					text = e.Delta == "hello"
				}
			}
			if !started || !thought || !text || listCalls.Load() != 2 || streamCalls.Load() != 1 {
				t.Fatalf("start=%v thought=%v text=%v lists=%d streams=%d", started, thought, text, listCalls.Load(), streamCalls.Load())
			}
			after, err := provider.FindModel("google", builtin.ID)
			if err != nil || !reflect.DeepEqual(builtin, after) {
				t.Fatalf("built-in model changed: %+v, %v", after, err)
			}
		})
	}
}
