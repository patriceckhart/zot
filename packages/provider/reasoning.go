package provider

import "strings"

var reasoningLevelOrder = []string{"", "minimum", "low", "medium", "high", "xhigh", "max"}

// AvailableReasoningLevels returns the distinct reasoning levels supported by
// a model. Optional per-model overrides can remove, remap, or extend protocol
// defaults. The empty string represents off.
func AvailableReasoningLevels(model Model) []string {
	defaults := defaultReasoningLevels(model)
	if !model.Reasoning || len(model.ReasoningLevelMap) == 0 {
		return defaults
	}

	available := map[string]bool{"": containsReasoningLevel(defaults, "")}
	for _, level := range reasoningLevelOrder[1:] {
		if !containsReasoningLevel(defaults, level) {
			if _, overridden := model.ReasoningLevelMap[level]; !overridden {
				continue
			}
		}
		effective := level
		if mapped, overridden := model.ReasoningLevelMap[level]; overridden {
			effective = NormalizeReasoning(mapped)
		}
		if reasoningLevelRank(effective) > 0 {
			available[effective] = true
		}
	}

	levels := []string{}
	for _, level := range reasoningLevelOrder {
		if available[level] {
			levels = append(levels, level)
		}
	}
	return levels
}

func defaultReasoningLevels(model Model) []string {
	if !model.Reasoning {
		return []string{""}
	}

	id := strings.ToLower(model.ID)
	// Opus 5.5 and Sonnet 5.5 always use adaptive thinking, including at
	// their lowest effort.
	if alwaysOnAdaptiveThinking(id) {
		return []string{"low", "medium", "high", "xhigh", "max"}
	}
	if (model.Provider == "google" || model.Provider == "google-vertex") && strings.Contains(id, "gemini-3") {
		if strings.Contains(id, "-pro") {
			return []string{"", "low", "high"}
		}
		return []string{"", "minimum", "low", "medium", "high"}
	}
	if model.Provider == "deepseek" {
		// DeepSeek maps medium and xhigh to high; max is a distinct effort.
		return []string{"", "low", "high", "max"}
	}
	if model.AdaptiveThinkingCompat {
		return []string{"", "high"}
	}
	if model.AdaptiveThinking {
		return []string{"", "low", "medium", "high", "xhigh", "max"}
	}
	if model.API == APIResponses || model.Provider == "openai-codex" || model.Provider == "openai-responses" || model.Provider == "azure-openai-responses" {
		levels := []string{"", "low", "medium", "high", "xhigh"}
		if supportsResponsesMaxEffort(id) {
			levels = append(levels, "max")
		}
		return levels
	}
	if model.Provider == "google" || model.Provider == "google-vertex" {
		if strings.Contains(id, "gemini-2.5") {
			return []string{"", "minimum", "low", "medium", "high", "xhigh"}
		}
		return []string{""}
	}
	if usesReasoningBudget(model) {
		return []string{"", "minimum", "low", "medium", "high", "xhigh"}
	}
	if model.Provider == "amazon-bedrock" {
		return []string{""}
	}
	return []string{"", "low", "medium", "high"}
}

// alwaysOnAdaptiveThinking reports whether a Claude model rejects thinking
// being turned off. Both the hyphenated Anthropic ID and the dotted Copilot
// ID are recognised, as are Bedrock IDs ("anthropic." plus an optional
// inference-profile prefix such as "us." or "global.").
func alwaysOnAdaptiveThinking(id string) bool {
	if i := strings.Index(id, "anthropic."); i >= 0 && !strings.Contains(id[:i], "/") {
		id = id[i+len("anthropic."):]
	}
	switch id {
	case "claude-opus-5-5", "claude-opus-5.5", "claude-sonnet-5-5", "claude-sonnet-5.5":
		return true
	}
	return false
}

func containsReasoningLevel(levels []string, target string) bool {
	for _, level := range levels {
		if level == target {
			return true
		}
	}
	return false
}

func hasReasoningLevelOverride(model Model, level string) bool {
	_, ok := model.ReasoningLevelMap[NormalizeReasoning(level)]
	return ok
}

// ClampReasoningForModel maps a configured level to the nearest level exposed
// for the active model. Ties prefer the higher level.
func ClampReasoningForModel(model Model, level string) string {
	normalized := NormalizeReasoning(level)
	available := AvailableReasoningLevels(model)
	if mapped, overridden := model.ReasoningLevelMap[normalized]; overridden {
		target := NormalizeReasoning(mapped)
		if target != "" && containsReasoningLevel(available, target) {
			return target
		}
	}
	for _, candidate := range available {
		if candidate == normalized {
			return candidate
		}
	}
	if model.Provider == "deepseek" && (normalized == "medium" || normalized == "xhigh") && containsReasoningLevel(available, "high") {
		return "high"
	}
	return nearestReasoningLevel(available, normalized)
}

func nearestReasoningLevel(available []string, requested string) string {
	if len(available) == 0 {
		return ""
	}
	if requested == "" || len(available) == 1 {
		return available[0]
	}
	requestedRank := reasoningLevelRank(requested)
	if requestedRank == 0 {
		return available[0]
	}
	best, bestDistance := available[0], len(reasoningLevelOrder)
	for _, candidate := range available {
		if candidate == "" {
			continue
		}
		distance := reasoningLevelRank(candidate) - requestedRank
		if distance < 0 {
			distance = -distance
		}
		if distance <= bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

func reasoningLevelRank(level string) int {
	for rank, candidate := range reasoningLevelOrder {
		if candidate == level {
			return rank
		}
	}
	return 0
}

func usesReasoningBudget(model Model) bool {
	switch model.Provider {
	case "anthropic", "fireworks", "kimi", "minimax", "minimax-cn", "vercel-ai-gateway":
		return true
	}
	return model.API == "anthropic"
}

// NormalizeReasoning canonicalizes zot's user-facing reasoning levels.
// Empty string means reasoning is disabled. "maximum" remains
// an alias for xhigh; "max" is the separate opt-in tier above it.
func NormalizeReasoning(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "", "off", "none", "no", "false", "disabled":
		return ""
	case "min", "minimal", "minimum":
		return "minimum"
	case "low":
		return "low"
	case "med", "medium":
		return "medium"
	case "hi", "high":
		return "high"
	case "xhigh", "maximum":
		return "xhigh"
	case "max":
		return "max"
	default:
		return strings.ToLower(strings.TrimSpace(level))
	}
}

// ReasoningBudget returns zot's approximate token budget for reasoning-capable
// providers that accept explicit budgets.
func ReasoningBudget(level string) int {
	switch NormalizeReasoning(level) {
	case "minimum":
		return 1024
	case "low":
		return 2048
	case "medium":
		return 8192
	case "high":
		return 16384
	case "xhigh", "max":
		return 32768
	default:
		return 0
	}
}

// AnthropicAdaptiveEffort maps zot's user-facing reasoning levels onto the
// effort enum used by adaptive-thinking models. These models reject explicit
// thinking budgets; reasoning depth is controlled by output_config.effort.
func AnthropicAdaptiveEffort(level string) string {
	switch NormalizeReasoning(level) {
	case "minimum", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "xhigh"
	case "max":
		return "max"
	default:
		return ""
	}
}

// OpenAIReasoningEffort maps zot's thinking setting onto the effort enum
// accepted by generic OpenAI-compatible chat-completions endpoints.
func OpenAIReasoningEffort(level string) string {
	switch NormalizeReasoning(level) {
	case "minimum", "low":
		// Many compatible endpoints only accept low/medium/high.
		return "low"
	case "medium":
		return "medium"
	case "high", "xhigh", "max":
		return "high"
	default:
		return ""
	}
}

// OpenAICompatAnthropicEffort maps zot's thinking setting when an adaptive
// Anthropic model is served over an OpenAI-compatible chat-completions wire.
// Adaptive models accept native xhigh and max effort values.
func OpenAICompatAnthropicEffort(level string) string {
	switch NormalizeReasoning(level) {
	case "minimum", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "xhigh"
	case "max":
		return "max"
	default:
		return ""
	}
}

// OpenAICodexReasoningEffort maps zot levels onto the Responses API effort
// enum. GPT-5.6, GPT-6 and GPT-6.1 Sol support native max; other models clamp max to xhigh.
func OpenAICodexReasoningEffort(level, model string) string {
	switch NormalizeReasoning(level) {
	case "minimum", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "xhigh"
	case "max":
		if supportsResponsesMaxEffort(model) {
			return "max"
		}
		return "xhigh"
	default:
		return ""
	}
}

func supportsResponsesMaxEffort(model string) bool {
	id := strings.ToLower(model)
	return strings.HasPrefix(id, "gpt-5.6-") || id == "gpt-6-astra" || id == "gpt-6-sol" || id == "gpt-6-luna" || id == "gpt-6.1-sol"
}
