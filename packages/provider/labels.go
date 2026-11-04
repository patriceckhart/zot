package provider

import "strings"

// ProviderLabel returns the user-facing label for a provider id.
func ProviderLabel(id string) string {
	switch id {
	case "anthropic":
		return "Anthropic (Claude Pro/Max)"
	case "openai":
		return "OpenAI"
	case LMStudioProviderID:
		return "LM Studio"
	case "openai-codex":
		return "OpenAI Codex (ChatGPT Plus/Pro)"
	case "openai-responses":
		return "OpenAI Responses"
	case "kimi":
		return "Kimi Code"
	case "deepseek":
		return "DeepSeek"
	case "google":
		return "Google (Gemini API key)"
	case "github-copilot":
		return "GitHub Copilot"
	case "moonshotai":
		return "Moonshot AI"
	case "moonshotai-cn":
		return "Moonshot AI CN"
	case "groq":
		return "Groq"
	case "xai":
		return "xAI"
	case "cerebras":
		return "Cerebras"
	case "together":
		return "Together AI"
	case "huggingface":
		return "Hugging Face"
	case "openrouter":
		return "OpenRouter"
	case "gondola":
		return "Gondola"
	case "yolo-auto":
		return "Yolo-Auto"
	case "cheaperinference":
		return "Cheaper Inference"
	case "opper":
		return "Opper"
	case "runcrate":
		return "Runcrate"
	case "cloudgpu":
		return "CloudGPU"
	case "mistral":
		return "Mistral"
	case "zai":
		return "Z.AI"
	case "xiaomi":
		return "Xiaomi"
	case "xiaomi-token-plan-ams":
		return "Xiaomi Token Plan AMS"
	case "xiaomi-token-plan-cn":
		return "Xiaomi Token Plan CN"
	case "xiaomi-token-plan-sgp":
		return "Xiaomi Token Plan SGP"
	case "minimax":
		return "MiniMax"
	case "minimax-cn":
		return "MiniMax CN"
	case "fireworks":
		return "Fireworks"
	case "vercel-ai-gateway":
		return "Vercel AI Gateway"
	case "opencode":
		return "OpenCode"
	case "opencode-go":
		return "OpenCode Go"
	case "amazon-bedrock":
		return "Amazon Bedrock"
	case "google-vertex":
		return "Google Vertex AI"
	case "azure-openai-responses":
		return "Azure OpenAI"
	case "cloudflare-workers-ai":
		return "Cloudflare Workers AI"
	case "cloudflare-ai-gateway":
		return "Cloudflare AI Gateway"
	case "ollama":
		return "Ollama"
	case LlamaCPPProviderID:
		return "llama.cpp"
	}
	return titleProviderID(id)
}

func titleProviderID(id string) string {
	parts := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}
