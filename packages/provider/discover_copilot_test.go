package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestParseCopilotAvailableModels(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		fallback   bool
		want       []string
		invalid    bool
	}{
		{"picker", `{"data":[{"id":"yes","model_picker_enabled":true},{"id":"disabled","model_picker_enabled":true,"policy":{"state":"disabled"}},{"id":"hidden","policy":{"state":"enabled"}},{"id":"no-tools","model_picker_enabled":true,"capabilities":{"supports":{"tool_calls":false}}}]}`, true, []string{"yes"}, false},
		{"individual fallback", `{"data":[{"id":"enabled","policy":{"state":"enabled"}},{"id":"unconfigured","policy":{"state":"unconfigured"}},{"id":"disabled","policy":{"state":"disabled"}}]}`, true, []string{"enabled"}, false},
		{"enterprise no fallback", `{"data":[{"id":"enabled","policy":{"state":"enabled"}}]}`, false, []string{}, false},
		{"empty", `{"data":[]}`, true, []string{}, false},
		{"duplicate", `{"data":[{"id":"yes","model_picker_enabled":true},{"id":"yes","model_picker_enabled":true}]}`, false, []string{"yes"}, false},
		{"missing", `{}`, false, nil, true},
		{"null", `{"data":null}`, false, nil, true},
		{"invalid", `{"data":{}}`, false, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCopilotAvailableModels([]byte(tc.body), tc.fallback)
			if (err != nil) != tc.invalid {
				t.Fatalf("error = %v", err)
			}
			if !tc.invalid && !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCopilotDiscoveryHTTP(t *testing.T) {
	for _, status := range []int{200, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/models" {
					t.Error("incorrect discovery request")
				}
				if r.Header.Get("Authorization") != "Bearer synthetic-session-token" || r.Header.Get("Copilot-Integration-Id") != "vscode-chat" || r.Header.Get("X-GitHub-Api-Version") != "2026-06-01" {
					t.Error("incorrect discovery headers")
				}
				w.WriteHeader(status)
				if status != 200 {
					fmt.Fprint(w, "sensitive-response-body")
					return
				}
				fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-5","model_picker_enabled":true}]}`)
			}))
			defer srv.Close()
			pat := seedCopilotTestToken(t, srv.URL)
			got, err := DiscoverCopilotAvailableModels(context.Background(), pat)
			if status == 200 {
				if err != nil || !reflect.DeepEqual(got, []string{"claude-sonnet-5"}) {
					t.Fatalf("got %v, %v", got, err)
				}
			} else if err == nil || strings.Contains(err.Error(), "sensitive-response-body") {
				t.Fatalf("unexpected error %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := DiscoverCopilotAvailableModels(ctx, pat); err == nil {
				t.Error("cancellation ignored")
			}
		})
	}
}

func TestModelAvailabilityFiltersWithoutDeletingCatalog(t *testing.T) {
	t.Cleanup(func() { SetModelAvailability("github-copilot", nil) })
	before := len(ModelsForProvider("anthropic"))
	copilotBefore := len(ModelsForProvider("github-copilot"))
	ids := []string{"claude-sonnet-5"}
	SetModelAvailability("github-copilot", ids)
	ids[0] = "mutated"
	models := ModelsForProvider("github-copilot")
	if len(models) != 1 || models[0].ID != "claude-sonnet-5" {
		t.Fatalf("models = %v", models)
	}
	if len(ModelsForProvider("anthropic")) != before {
		t.Error("changed another provider")
	}
	SetModelAvailability("github-copilot", []string{})
	// Hiding a model must not change wire metadata for a session already using it.
	model, err := FindModel("github-copilot", "gpt-5.4")
	if err != nil || model.API != APIResponses {
		t.Fatalf("availability refresh lost routing metadata: %+v, %v", model, err)
	}
	if len(ModelsForProvider("github-copilot")) != 0 {
		t.Error("empty availability did not hide models")
	}
	SetModelAvailability("github-copilot", nil)
	if len(ModelsForProvider("github-copilot")) != copilotBefore {
		t.Error("clearing restriction did not restore catalog")
	}
}
