package swarm

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// MaxPromptWireBytes bounds encoded prompts, including base64 image data.
const MaxPromptWireBytes = 8 << 20

// Prompt is an additive supervisor payload for user turns with images.
// Legacy text-only user messages remain supported.
type Prompt struct {
	// RequestID correlates automated follow-ups with their prompt-level result.
	RequestID string                `json:"request_id,omitempty"`
	Text      string                `json:"text"`
	Images    []provider.ImageBlock `json:"images,omitempty"`
}

// WIPContext snapshots the parent's draft boundary and prompt settings.
type WIPContext struct {
	State    core.WIPState    `json:"state"`
	Settings core.WIPSettings `json:"settings"`
}

// StartupInput shares a single stdin object with credential inheritance.
// Keeping the prompt off argv also avoids command-line image size limits.
type StartupInput struct {
	WIP *WIPContext `json:"wip,omitempty"`
	Credential
	Prompt *Prompt `json:"prompt,omitempty"`
}

// PreparePrompt validates image bytes and takes ownership of a copy.
func PreparePrompt(text string, images []provider.ImageBlock) (Prompt, error) {
	text, copied, err := continuous.PrepareSubmission(text, images, nil)
	if err != nil {
		return Prompt{}, err
	}
	return Prompt{Text: text, Images: copied}, nil
}

// EncodePrompt frames a bounded image-bearing prompt as one inbox line.
func EncodePrompt(text string, images []provider.ImageBlock) (string, error) {
	prompt, err := PreparePrompt(text, images)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(prompt)
	if err != nil {
		return "", fmt.Errorf("swarm: encode prompt")
	}
	if len(data) > MaxPromptWireBytes {
		return "", fmt.Errorf("swarm: encoded prompt exceeds %d bytes", MaxPromptWireBytes)
	}
	return "user-prompt " + string(data), nil
}

// DecodePrompt accepts legacy text messages and additive image payloads.
// Errors deliberately omit payload data, which may contain private content.
func DecodePrompt(msg string) (Prompt, error) {
	if strings.HasPrefix(msg, "user ") {
		return Prompt{Text: strings.TrimPrefix(msg, "user ")}, nil
	}
	if !strings.HasPrefix(msg, "user-prompt ") || len(msg) > MaxPromptWireBytes+len("user-prompt ") {
		return Prompt{}, fmt.Errorf("swarm: invalid prompt frame")
	}
	var prompt Prompt
	if err := json.Unmarshal([]byte(strings.TrimPrefix(msg, "user-prompt ")), &prompt); err != nil {
		return Prompt{}, fmt.Errorf("swarm: invalid prompt encoding")
	}
	if strings.TrimSpace(prompt.Text) == "" && len(prompt.Images) == 0 {
		return Prompt{}, fmt.Errorf("swarm: empty prompt")
	}
	if len(prompt.RequestID) > 128 || strings.ContainsAny(prompt.RequestID, "\r\n") {
		return Prompt{}, fmt.Errorf("swarm: invalid request id")
	}
	prepared, err := PreparePrompt(prompt.Text, prompt.Images)
	prepared.RequestID = prompt.RequestID
	return prepared, err
}
