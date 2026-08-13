package sdk

import "testing"

func TestArgsFromConfigPreservesExplicitEmptySystemPrompt(t *testing.T) {
	args := argsFromConfig(Config{SystemPromptSet: true})
	if !args.SystemPromptSet {
		t.Fatal("SystemPromptSet = false; want true for an explicitly empty SDK prompt")
	}
	if args.SystemPrompt != "" {
		t.Fatalf("SystemPrompt = %q; want empty", args.SystemPrompt)
	}
}

func TestArgsFromConfigTreatsNonEmptySystemPromptAsSet(t *testing.T) {
	args := argsFromConfig(Config{SystemPrompt: "custom"})
	if !args.SystemPromptSet {
		t.Fatal("SystemPromptSet = false; want true for a non-empty SDK prompt")
	}
}
