package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestAuxiliaryCatalogCoverageAndIsolation(t *testing.T) {
	seen := map[string]bool{}
	images, classifiers := 0, 0
	providers := map[string]bool{}
	for _, model := range builtinNonChatModels() {
		key := model.Type + "/" + model.Provider + "/" + model.ID
		if seen[key] || model.ID == "" || model.API == "" || model.BaseURL == "" || len(model.Input) == 0 {
			t.Fatalf("invalid entry: %+v", model)
		}
		seen[key] = true
		providers[model.Provider] = true
		if model.Type == "image" {
			images++
			if model.API != "openrouter-images" {
				t.Fatal(model)
			}
		} else if model.Type == "classifier" {
			classifiers++
		} else {
			t.Fatal(model)
		}
	}
	if images < 50 || classifiers < 15 {
		t.Fatalf("incomplete auxiliary catalog: %d images, %d classifiers", images, classifiers)
	}
	for _, provider := range []string{"typesafe", "openrouter", "opencode", "vercel-ai-gateway", "cloudflare-workers-ai"} {
		if !providers[provider] {
			t.Fatal(provider)
		}
	}
	for _, model := range Active() {
		if model.Provider == "typesafe" && model.ID == "jev-latest" {
			t.Fatal("classifier leaked into chat picker")
		}
	}
}

func TestCloudflareClassifierAccountResolution(t *testing.T) {
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "synthetic-account")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/accounts/synthetic-account/ai/run" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-key" {
			t.Error("missing synthetic authorization")
		}
		var payload struct {
			Model string
			Input ClassifierContext
		}
		if json.NewDecoder(r.Body).Decode(&payload) != nil || payload.Model != "typesafe/jev" || len(payload.Input.Questions) != 1 {
			t.Error("invalid classifier envelope")
		}
		_, _ = w.Write([]byte(`{"success":true,"result":{"state":"Completed","result":{"answers":{"q":{"type":"noul","noul":0.75}}}}}`))
	}))
	defer server.Close()
	input, err := ParseClassifierContext(json.RawMessage(`{"state":{},"questions":{"q":{"type":"bool","instructions":"Ready?","criteria":{"true":"ready","false":"not ready"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	model := NonChatModel{Model: Model{Provider: "cloudflare-workers-ai", ID: "typesafe/jev", API: "cloudflare-workers-ai-system-one", BaseURL: server.URL + "/accounts/{CLOUDFLARE_ACCOUNT_ID}/ai"}, Type: "classifier"}
	result, _ := (NonChatClient{}).Classify(context.Background(), model, input, "synthetic-key")
	if result["stopReason"] != "stop" {
		t.Fatal(result)
	}
	t.Setenv("CLOUDFLARE_ACCOUNT_ID", "")
	result, _ = (NonChatClient{}).Classify(context.Background(), model, input, "synthetic-key")
	if result["stopReason"] != "error" || !strings.Contains(result["errorMessage"].(string), "CLOUDFLARE_ACCOUNT_ID") {
		t.Fatal(result)
	}
}

func TestLocalClassificationPromptAndLabelOrder(t *testing.T) {
	raw := json.RawMessage(`{"state":{"z":1,"a":2},"questions":{"second":{"type":"choice","instructions":"Pick","criteria":{"z":"last","a":"first"}},"first":{"type":"bool","instructions":"Ready?","criteria":{"true":"ready","false":""}}}}`)
	input, err := ParseClassifierContext(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(localQuestionIDs(input), []string{"second", "first"}) {
		t.Fatal(localQuestionIDs(input))
	}
	labels, keys, _, err := localLabels(input.Questions["second"])
	if err != nil || !reflect.DeepEqual(keys, []string{"z", "a"}) {
		t.Fatalf("labels %v keys %v: %v", labels, keys, err)
	}
	prompt, err := localQuestionPrompt(input, "second", labels)
	if err != nil {
		t.Fatal(err)
	}
	expectedState := "State:\n{\n \"z\": 1,\n \"a\": 2\n}"
	if strings.Count(prompt, expectedState) != 2 || !strings.Contains(prompt, "Options:\n- z: last\n- a: first") || !strings.HasSuffix(prompt, "Options:\nA. z: last\nB. a: first\n\nAnswer with one letter.") {
		t.Fatal(prompt)
	}
	if got := jsonObjectKeys(json.RawMessage(`{"z":0,"10":0,"2":0,"a":0,"01":0}`)); !reflect.DeepEqual(got, []string{"2", "10", "z", "a", "01"}) {
		t.Fatal(got)
	}
}

func TestLocalTokenObjects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"tokens":[{"id":12},13]}`)) }))
	defer server.Close()
	tokens, err := (NonChatClient{}).labelTokens(context.Background(), server.URL, "synthetic", "local", "Yes")
	if err != nil || !reflect.DeepEqual(tokens, []int{12, 13}) {
		t.Fatalf("%v %v", tokens, err)
	}
}
