package agent

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/patriceckhart/zot/packages/agent/swarm"
)

// readSwarmStartupInput decodes one stdin object so credential decoding cannot
// buffer and consume a separate image payload. Legacy credential input keeps
// its original shape, the optional prompt field is additive.
func readSwarmStartupInput(r io.Reader, credential, prompt bool, args *Args) (swarm.Prompt, error) {
	initial := swarm.Prompt{Text: args.Prompt}
	if !credential && !prompt {
		return initial, nil
	}
	limit := int64(1 << 20)
	if prompt {
		limit = swarm.MaxPromptWireBytes
	}
	var input swarm.StartupInput
	if err := json.NewDecoder(io.LimitReader(r, limit)).Decode(&input); err != nil {
		return swarm.Prompt{}, fmt.Errorf("read swarm startup input: invalid encoding")
	}
	if credential {
		if input.Value == "" || input.Method == "" {
			return swarm.Prompt{}, fmt.Errorf("read inherited swarm credential: missing value or method")
		}
		args.inheritedCredential = input.Value
		args.inheritedAuthMethod = input.Method
		args.inheritedAccountID = input.AccountID
	}
	if prompt {
		if input.Prompt == nil {
			return swarm.Prompt{}, fmt.Errorf("read swarm startup input: missing prompt")
		}
		var err error
		initial, err = swarm.PreparePrompt(input.Prompt.Text, input.Prompt.Images)
		if err != nil {
			return swarm.Prompt{}, fmt.Errorf("read swarm startup attachments: %w", err)
		}
		if initial.Text == "" && len(initial.Images) == 0 {
			return swarm.Prompt{}, fmt.Errorf("read swarm startup input: empty prompt")
		}
	}
	return initial, nil
}
