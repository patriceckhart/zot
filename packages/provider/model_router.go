package provider

import (
	"context"
	"fmt"
)

const (
	// APIAnthropicMessages identifies the Anthropic Messages wire API.
	APIAnthropicMessages = "anthropic-messages"
	// APICompletions identifies the OpenAI Chat Completions wire API.
	APICompletions = "openai-completions"
	// APIResponses identifies the OpenAI Responses wire API.
	APIResponses = "openai-responses"
)

// modelRouter dispatches each request according to the selected model's API.
// This lets one provider expose models that use different wire protocols while
// remaining reusable across model switches.
type modelRouter struct {
	name     string
	fallback Client
	byAPI    map[string]Client
}

// NewModelRouter creates a client that dispatches requests using Model.API.
// Models with no API override use fallback. Models absent from the catalog
// also use fallback, except xAI selections awaiting discovery use Responses.
func NewModelRouter(name string, fallback Client, byAPI map[string]Client) Client {
	return &modelRouter{name: name, fallback: fallback, byAPI: byAPI}
}

func (c *modelRouter) Name() string { return c.name }

func (c *modelRouter) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	client := c.fallback
	model, err := FindModel(c.name, req.Model)
	if err != nil && c.name == "xai" {
		model, err = UnlistedXAIModel(req.Model), nil
	}
	if err == nil && model.API != "" {
		routed := c.byAPI[model.API]
		if routed == nil {
			return nil, fmt.Errorf("provider %q has no client for model API %q", c.name, model.API)
		}
		client = routed
	}
	return client.Stream(ctx, req)
}
