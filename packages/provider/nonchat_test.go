package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const boolContext = `{"state":{"text":"synthetic"},"questions":{"ok":{"type":"bool","instructions":"Is it valid?","criteria":{"true":"valid","false":"invalid"}}}}`

func TestClassifierTransports(t *testing.T) {
	for _, api := range []string{"typesafe-system-one", "cloudflare-workers-ai-system-one"} {
		t.Run(api, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic" {
					t.Error("missing authorization")
				}
				var body struct {
					Model     string                        `json:"model"`
					Questions map[string]ClassifierQuestion `json:"questions"`
					Input     ClassifierContext             `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				questions := body.Questions
				if api == "cloudflare-workers-ai-system-one" {
					questions = body.Input.Questions
					if r.URL.Path != "/run" {
						t.Error(r.URL.Path)
					}
				} else if r.URL.Path != "/systemone" {
					t.Error(r.URL.Path)
				}
				if body.Model != "test" || questions["ok"].Type != "noul" {
					t.Errorf("bad wire request: %+v", body)
				}
				result := `{"answers":{"ok":{"type":"noul","noul":0.75}},"usage":{"input_tokens":10,"output_tokens":2}}`
				if api == "cloudflare-workers-ai-system-one" {
					result = `{"success":true,"result":{"state":"Completed","result":` + result + `}}`
				}
				fmt.Fprint(w, result)
			}))
			defer server.Close()
			input, err := ParseClassifierContext(json.RawMessage(boolContext))
			if err != nil {
				t.Fatal(err)
			}
			model := NonChatModel{Model: Model{API: api, ID: "test", Provider: "synthetic", BaseURL: server.URL, PriceInput: 1, PriceOutput: 2}, Type: "classifier"}
			result, usage := (NonChatClient{HTTP: server.Client()}).Classify(context.Background(), model, input, "synthetic")
			answers := result["answers"].(map[string]any)
			if result["stopReason"] != "stop" || usage == nil || usage.InputTokens != 10 || usage.OutputTokens != 2 || answers["ok"].(map[string]any)["probability"] != 0.75 {
				t.Fatalf("result=%+v usage=%+v", result, usage)
			}
		})
	}
}

func TestClassifierMalformedResponseKeepsUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"answers":{"ok":{"type":"noul","noul":null}},"usage":{"input_tokens":8}}`)
	}))
	defer server.Close()
	input, _ := ParseClassifierContext(json.RawMessage(boolContext))
	result, usage := (NonChatClient{HTTP: server.Client()}).Classify(context.Background(), NonChatModel{Model: Model{API: "typesafe-system-one", BaseURL: server.URL}}, input, "synthetic")
	if result["stopReason"] != "error" || usage == nil || usage.InputTokens != 8 {
		t.Fatalf("%+v %+v", result, usage)
	}
}

func TestGenerateImagesWireAndFailurePrivacy(t *testing.T) {
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "synthetic-secret")
			return
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Error("invalid request")
		}
		if r.URL.Path != "/chat/completions" || string(body["stream"]) != "false" || string(body["modalities"]) != `["image","text"]` {
			t.Errorf("bad request: %s %+v", r.URL.Path, body)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"caption","images":[{"image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":3,"cost":0.02}}`)
	}))
	defer server.Close()
	input, err := ParseImagesContext(json.RawMessage(`{"input":[{"type":"text","text":"a drawing"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	model := NonChatModel{Model: Model{API: "openrouter-images", ID: "test", BaseURL: server.URL}, Output: []string{"image", "text"}}
	client := NonChatClient{HTTP: server.Client()}
	result, usage := client.GenerateImages(context.Background(), model, input, "synthetic-secret")
	output := result["output"].([]map[string]any)
	if result["stopReason"] != "stop" || len(output) != 2 || output[1]["mimeType"] != "image/png" || usage == nil || usage.CostUSD != 0.02 {
		t.Fatalf("%+v %+v", result, usage)
	}
	fail.Store(true)
	result, _ = client.GenerateImages(context.Background(), model, input, "synthetic-secret")
	encoded, _ := json.Marshal(result)
	if result["stopReason"] != "error" || strings.Contains(string(encoded), "synthetic-secret") {
		t.Fatalf("%s", encoded)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, _ = client.GenerateImages(ctx, model, input, "synthetic")
	if result["stopReason"] != "aborted" {
		t.Fatalf("%+v", result)
	}
}

func TestLocalClassifierTokenProbabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		switch r.URL.Path {
		case "/tokenize":
			var content string
			_ = json.Unmarshal(body["content"], &content)
			switch content {
			case "\n":
				fmt.Fprint(w, `{"tokens":[1]}`)
			case "\nYes":
				fmt.Fprint(w, `{"tokens":[1,2]}`)
			case "\nNo":
				fmt.Fprint(w, `{"tokens":[1,3]}`)
			default:
				t.Errorf("unexpected tokenization %q", content)
			}
		case "/apply-template":
			fmt.Fprint(w, `{"prompt":"synthetic prompt"}`)
		case "/completion":
			if string(body["n_predict"]) != "1" || string(body["post_sampling_probs"]) != "false" {
				t.Error("not reading raw logits")
			}
			fmt.Fprint(w, `{"completion_probabilities":[{"top_logprobs":[{"id":2,"logprob":-1},{"id":3,"logprob":-2}]}]}`)
		default:
			t.Error(r.URL.Path)
		}
	}))
	defer server.Close()
	input, _ := ParseClassifierContext(json.RawMessage(boolContext))
	result, _ := (NonChatClient{HTTP: server.Client()}).Classify(context.Background(), NonChatModel{Model: Model{API: "llama-cpp-classify", BaseURL: server.URL + "/v1", ID: "test"}}, input, "local")
	if result["stopReason"] != "stop" {
		t.Fatalf("%+v", result)
	}
	probability := result["answers"].(map[string]any)["ok"].(map[string]any)["probability"].(float64)
	if math.Abs(probability-1/(1+math.Exp(-1))) > 1e-10 {
		t.Fatal(probability)
	}
}

func TestInvalidNonChatContexts(t *testing.T) {
	for _, raw := range []string{`null`, `{"state":{},"questions":{}}`, `{"state":{},"questions":{"a":{"type":"bool","instructions":"x","criteria":{"true":null,"false":"no"}}}}`, `{"state":{},"questions":{"a":{"type":"score","instructions":"x","criteria":[null]}}}`} {
		if _, err := ParseClassifierContext(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, raw := range []string{`null`, `{"input":[]}`, `{"input":[{"type":"image","data":"","mimeType":"image/png"}]}`, `{"input":[{"type":"text","text":null}]}`} {
		if _, err := ParseImagesContext(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
