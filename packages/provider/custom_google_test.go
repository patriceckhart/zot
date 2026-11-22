package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadUserModelsGoogleAPI(t *testing.T) {
	preserveUserModelRegistries(t)
	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(`{"providers":{"custom-google":{"api":"google","baseUrl":"https://gemini.example.com","models":[{"id":"gemini-3-pro","reasoning":true}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	models, warnings := LoadUserModelsWithWarnings(path)
	if len(warnings) != 0 || len(models) != 1 {
		t.Fatalf("models = %+v, warnings = %v", models, warnings)
	}
	if models[0].API != "google" || CustomProviders()["custom-google"].API != "google" {
		t.Fatal("Google wire format not preserved")
	}
}

func TestCustomGoogleReasoningLevels(t *testing.T) {
	for _, id := range []string{"gemini-3-pro", "gemini-3-flash", "gemini-2.5-pro"} {
		t.Run(id, func(t *testing.T) {
			builtin := Model{Provider: "google", ID: id, Reasoning: true}
			custom := Model{Provider: "custom-google", API: "google", ID: id, Reasoning: true}
			if got, want := AvailableReasoningLevels(custom), AvailableReasoningLevels(builtin); !reflect.DeepEqual(got, want) {
				t.Fatalf("levels = %v, want %v", got, want)
			}
			client := NewGeminiNamed("custom-google", "", "https://gemini.example.com").(*geminiClient)
			wire, _, err := client.buildRequest(Request{Model: id, Reasoning: "minimum"})
			if err != nil {
				t.Fatal(err)
			}
			want := geminiThinkingConfig(id, ClampReasoningForModel(custom, "minimum"))
			if !reflect.DeepEqual(wire.GenerationConfig.ThinkingConfig, want) {
				t.Fatalf("thinking config = %+v, want %+v", wire.GenerationConfig.ThinkingConfig, want)
			}
		})
	}
}

func TestDiscoverCustomGoogleErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http", http.StatusUnauthorized, "sensitive-response"},
		{"invalid-json", http.StatusOK, "sensitive-response"},
		{"wrong-format", http.StatusOK, `{"data":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			_, err := DiscoverGoogleNamed(context.Background(), "custom-google", "synthetic-key", server.URL)
			if err == nil || !strings.Contains(err.Error(), "custom-google") || strings.Contains(err.Error(), "sensitive-response") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := DiscoverGoogleNamed(ctx, "custom-google", "", "https://gemini.example.com"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	for _, baseURL := range []string{"", "file:///tmp/models"} {
		if _, err := DiscoverGoogleNamed(context.Background(), "custom-google", "", baseURL); err == nil {
			t.Fatalf("accepted invalid base URL %q", baseURL)
		}
	}
}
