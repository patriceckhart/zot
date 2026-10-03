package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/packages/agent/codemode"
	"github.com/patriceckhart/zot/packages/provider"
)

func newCodemodeModelCaller(sessionProvider, sessionKey, sessionBase string) codemode.ModelCaller {
	return func(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, *provider.Usage, error) {
		var args []json.RawMessage
		if err := json.Unmarshal(raw, &args); err != nil {
			return nil, nil, fmt.Errorf("model arguments must be JSON values")
		}
		stringArg := func(index int) (string, error) {
			if index >= len(args) {
				return "", fmt.Errorf("missing model argument")
			}
			var value *string
			if err := json.Unmarshal(args[index], &value); err != nil || value == nil {
				return "", fmt.Errorf("model argument must be a string")
			}
			return *value, nil
		}
		if method == "getModelsOfType" || method == "getAvailableOfType" || method == "getModelOfType" {
			kind, err := stringArg(0)
			if err != nil {
				return nil, nil, err
			}
			if kind != "chat" && kind != "classifier" && kind != "image" {
				return nil, nil, fmt.Errorf("model type must be chat, classifier or image")
			}
			var selectedProvider string
			if len(args) > 1 && string(args[1]) != "null" {
				selectedProvider, err = stringArg(1)
				if err != nil {
					return nil, nil, err
				}
			}
			var id string
			if method == "getModelOfType" {
				selectedProvider, err = stringArg(1)
				if err != nil {
					return nil, nil, err
				}
				id, err = stringArg(2)
				if err != nil {
					return nil, nil, err
				}
			}
			var models []provider.NonChatModel
			if kind == "chat" {
				for _, model := range provider.Known() {
					input := model.Input
					if len(input) == 0 {
						input = []string{"text"}
					}
					models = append(models, provider.NonChatModel{Model: model, Type: "chat", Input: input})
				}
			} else {
				models = provider.NonChatModels()
			}
			infos := make([]map[string]any, 0)
			availability := map[string]bool{}
			for _, model := range models {
				if model.Type != kind || selectedProvider != "" && model.Provider != selectedProvider || method == "getModelOfType" && (model.Provider != selectedProvider || model.ID != id) {
					continue
				}
				if method == "getAvailableOfType" {
					available, seen := availability[model.Provider]
					if !seen {
						available = sessionProvider == model.Provider && sessionKey != "" || provider.CustomProviders()[model.Provider].NoAuth || CredentialAvailable(model.Provider)
						availability[model.Provider] = available
					}
					if !available {
						continue
					}
				}
				info := nonChatModelInfo(model)
				if method == "getModelOfType" {
					value, err := json.Marshal(info)
					return value, nil, err
				}
				infos = append(infos, info)
			}
			if method == "getModelOfType" {
				return nil, nil, nil
			}
			value, err := json.Marshal(infos)
			return value, nil, err
		}
		if method != "classify" && method != "generateImages" {
			return nil, nil, fmt.Errorf("unknown model operation")
		}
		if len(args) < 2 {
			return nil, nil, fmt.Errorf("model inference requires a model and context")
		}
		var requested struct {
			Provider string `json:"provider"`
			ID       string `json:"id"`
		}
		if json.Unmarshal(args[0], &requested) != nil || requested.Provider == "" || requested.ID == "" {
			return nil, nil, fmt.Errorf("model must contain provider and id")
		}
		kind := "classifier"
		if method == "generateImages" {
			kind = "image"
		}
		var selected *provider.NonChatModel
		for _, model := range provider.NonChatModels() {
			if model.Type == kind && model.Provider == requested.Provider && model.ID == requested.ID {
				copy := model
				selected = &copy
				break
			}
		}
		if selected == nil {
			return nil, nil, fmt.Errorf("unknown %s model, chat models cannot run inside scripts", kind)
		}
		// Scripts cannot override endpoints, authorization or transport metadata.
		if selected.Provider == sessionProvider && sessionBase != "" {
			selected.BaseURL = sessionBase
		}
		key := ""
		if selected.BaseURL == "" && selected.Provider == provider.LlamaCPPProviderID {
			base, resolvedKey, err := resolveLlamaCPPConfig(ctx, apiKeyCommandExecute)
			if err != nil {
				return nil, nil, err
			}
			selected.BaseURL = base
			if base != "" {
				key = firstNonEmpty(resolvedKey, "local")
			}
		}
		if provider.CustomProviders()[selected.Provider].NoAuth {
			key = "local"
		} else if selected.Provider == sessionProvider && sessionKey != "" {
			key = sessionKey
		} else if key == "" {
			var err error
			key, _, err = ResolveCredentialContext(ctx, selected.Provider, "")
			if err != nil {
				key = ""
			}
		}
		client := provider.NonChatClient{}
		var value map[string]any
		var usage *provider.Usage
		if method == "classify" {
			input, err := provider.ParseClassifierContext(args[1])
			if err != nil {
				return nil, nil, err
			}
			value, usage = client.Classify(ctx, *selected, input, key)
		} else {
			input, err := provider.ParseImagesContext(args[1])
			if err != nil {
				return nil, nil, err
			}
			value, usage = client.GenerateImages(ctx, *selected, input, key)
		}
		encoded, err := json.Marshal(value)
		return encoded, usage, err
	}
}

func nonChatModelInfo(model provider.NonChatModel) map[string]any {
	name := model.DisplayName
	if name == "" {
		name = model.ID
	}
	info := map[string]any{"type": model.Type, "provider": model.Provider, "id": model.ID, "name": name, "api": model.API, "input": model.Input}
	if model.API == "" {
		info["api"] = "openai"
		if strings.HasPrefix(model.Provider, "anthropic") {
			info["api"] = "anthropic"
		}
	}
	info["reasoning"] = model.Reasoning
	info["cost"] = map[string]float64{"input": model.PriceInput, "output": model.PriceOutput, "cacheRead": model.PriceCacheRead, "cacheWrite": model.PriceCacheWrite}
	if model.MaxOutput > 0 {
		info["maxTokens"] = model.MaxOutput
	}
	if model.ContextWindow > 0 {
		info["contextWindow"] = model.ContextWindow
	}
	if len(model.Output) > 0 {
		info["output"] = model.Output
	}
	return info
}
