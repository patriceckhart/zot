package provider

import (
	"slices"
	"testing"
)

func TestBedrockClaude55Catalog(t *testing.T) {
	for _, tc := range []struct {
		family                       string
		in, out, cacheRead, cacheWrt float64
		levels                       []string
	}{
		{"opus", 4, 20, 0.2, 8, []string{"low", "medium", "high", "xhigh", "max"}},
		{"sonnet", 2, 10, 0.1, 4, []string{"low", "medium", "high", "xhigh", "max"}},
		{"haiku", 0.1, 0.5, 0.01, 0.2, []string{"", "low", "medium", "high", "xhigh", "max"}},
	} {
		for _, prefix := range []string{"", "us.", "eu.", "au.", "jp.", "global."} {
			id := prefix + "anthropic.claude-" + tc.family + "-5-5"
			m, err := FindModel("amazon-bedrock", id)
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
