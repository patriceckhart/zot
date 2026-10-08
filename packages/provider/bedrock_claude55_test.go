package provider

import (
	"slices"
	"strings"
	"testing"
)

func TestBedrockClaude55Catalog(t *testing.T) {
	for _, tc := range []struct {
		family                       string
		in, out, cacheRead, cacheWrt float64
		levels                       []string
	}{
		{"opus", 4, 20, 0.2, 5, []string{"low", "medium", "high", "xhigh", "max"}},
		{"sonnet", 2, 10, 0.1, 2.5, []string{"low", "medium", "high", "xhigh", "max"}},
		{"haiku", 0.1, 0.5, 0.01, 0.125, []string{"", "low", "medium", "high", "xhigh", "max"}},
	} {
		for _, prefix := range []string{"", "us.", "eu.", "au.", "jp.", "global."} {
			id := prefix + "anthropic.claude-" + tc.family + "-5-5"
			m, err := FindModel("amazon-bedrock", id)
			if tc.family == "sonnet" && (prefix == "au." || prefix == "jp.") {
				if err == nil {
					t.Errorf("%s: unsupported profile must not be in the catalog", id)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s: %v", id, err)
			}
			if m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Reasoning || !m.AdaptiveThinking {
				t.Errorf("%s: unexpected capabilities: %+v", id, m)
			}
			if m.PriceInput != tc.in || m.PriceOutput != tc.out || m.PriceCacheRead != tc.cacheRead || m.PriceCacheWrite != tc.cacheWrt {
				t.Errorf("%s: unexpected pricing: %+v", id, m)
			}
			if got := AvailableReasoningLevels(m); !slices.Equal(got, tc.levels) {
				t.Errorf("%s: levels = %q, want %q", id, got, tc.levels)
			}
			if !bedrockModelSupportsCaching(id) {
				t.Errorf("%s: caching should be enabled", id)
			}
		}
	}
}

func TestBedrockClaude55BuildRequest(t *testing.T) {
	client := &bedrockClient{region: "eu-central-1"}
	temperature := float32(0.5)
	msgs := []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}}

	// Sonnet 5.5 keeps adaptive thinking on even when off is requested.
	req, err := client.buildRequest(Request{Model: "anthropic.claude-sonnet-5-5", Reasoning: "off", Temperature: &temperature, Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if req.InferenceConfig.Temperature != nil {
		t.Fatal("adaptive thinking must omit temperature")
	}
	output, _ := req.AdditionalModelRequestFields["output_config"].(map[string]interface{})
	if output["effort"] != "low" {
		t.Fatalf("sonnet off: output config = %#v, want effort low", output)
	}

	// Haiku 5.5 allows thinking to be turned off.
	req, err = client.buildRequest(Request{Model: "anthropic.claude-haiku-5-5", Reasoning: "off", Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if req.AdditionalModelRequestFields != nil {
		t.Fatalf("haiku off: unexpected fields %#v", req.AdditionalModelRequestFields)
	}
	req, err = client.buildRequest(Request{Model: "anthropic.claude-haiku-5-5", Reasoning: "max", Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	output, _ = req.AdditionalModelRequestFields["output_config"].(map[string]interface{})
	if output["effort"] != "max" {
		t.Fatalf("haiku max: output config = %#v", output)
	}
}

func TestBedrockClaude55RegionalRequests(t *testing.T) {
	for _, tc := range []struct {
		region string
		prefix string
	}{
		{"us-east-1", "us."},
		{"eu-central-1", "eu."},
		{"ap-southeast-2", "au."},
		{"ap-southeast-4", "au."},
		{"ap-northeast-1", "jp."},
		{"ap-northeast-3", "jp."},
	} {
		for _, family := range []string{"sonnet", "haiku"} {
			t.Run(tc.region+"/"+family, func(t *testing.T) {
				id := "anthropic.claude-" + family + "-5-5"
				client := &bedrockClient{region: tc.region}
				temperature := float32(0.5)
				req, err := client.buildRequest(Request{Model: id, Reasoning: "high", Temperature: &temperature, System: "system", Messages: []Message{{Role: RoleUser, Content: []Content{TextBlock{Text: "hi"}}}}})
				if family == "sonnet" && (tc.prefix == "au." || tc.prefix == "jp.") {
					if err == nil || !strings.Contains(err.Error(), "explicit inference profile") {
						t.Fatalf("unsupported geography: error = %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if got := resolveBedrockInferenceProfileID(id, tc.region); got != tc.prefix+id {
					t.Fatalf("resolved ID = %q, want %q", got, tc.prefix+id)
				}
				thinking, _ := req.AdditionalModelRequestFields["thinking"].(map[string]interface{})
				output, _ := req.AdditionalModelRequestFields["output_config"].(map[string]interface{})
				if thinking["type"] != "adaptive" || output["effort"] != "high" || req.InferenceConfig.Temperature != nil {
					t.Fatalf("thinking configuration lost: %+v", req)
				}
				cachePoint, _ := req.System[1]["cachePoint"].(map[string]interface{})
				if cachePoint["type"] != "default" || cachePoint["ttl"] != nil {
					t.Fatalf("expected default 5-minute caching: %#v", cachePoint)
				}
			})
		}
	}
}

func TestBedrockClaude55ExplicitProfiles(t *testing.T) {
	for _, family := range []string{"sonnet", "haiku"} {
		base := "anthropic.claude-" + family + "-5-5"
		client := &bedrockClient{region: "ap-southeast-1"}
		if _, err := client.buildRequest(Request{Model: base, Reasoning: "high"}); err == nil {
			t.Errorf("%s: unsupported geography must require an explicit profile", base)
		}
		for _, id := range []string{"global." + base, "eu." + base, "arn:aws:bedrock:ap-southeast-1:123:application-inference-profile/example"} {
			if got := resolveBedrockInferenceProfileID(id, client.region); got != id {
				t.Errorf("explicit ID changed: %q -> %q", id, got)
			}
			if _, err := client.buildRequest(Request{Model: id, Reasoning: "high"}); err != nil {
				t.Errorf("explicit %s: %v", id, err)
			}
		}
	}
}
