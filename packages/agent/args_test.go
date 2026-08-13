package agent

import "testing"

func TestParseArgsExplicitEmptySystemPromptIsSet(t *testing.T) {
	args, err := ParseArgs([]string{"--system-prompt", ""})
	if err != nil {
		t.Fatalf("ParseArgs returned %v", err)
	}
	if !args.SystemPromptSet {
		t.Fatal("SystemPromptSet = false; want true for an explicitly empty flag value")
	}
	if args.SystemPrompt != "" {
		t.Fatalf("SystemPrompt = %q; want empty", args.SystemPrompt)
	}
}

func TestParseArgsTemperatureAllowsZero(t *testing.T) {
	args, err := ParseArgs([]string{"--temperature", "0"})
	if err != nil {
		t.Fatalf("ParseArgs returned %v", err)
	}
	if args.Temperature == nil || *args.Temperature != 0 {
		t.Fatalf("Temperature = %v; want 0", args.Temperature)
	}
}

func TestParseArgsExplicitEmptySystemPromptIsSet(t *testing.T) {
	args, err := ParseArgs([]string{"--system-prompt", ""})
	if err != nil {
		t.Fatalf("ParseArgs returned %v", err)
	}
	if !args.SystemPromptSet {
		t.Fatal("SystemPromptSet = false; want true for an explicitly empty flag value")
	}
	if args.SystemPrompt != "" {
		t.Fatalf("SystemPrompt = %q; want empty", args.SystemPrompt)
	}
}

func TestParseArgsTemperatureRejectsOutOfRange(t *testing.T) {
	if _, err := ParseArgs([]string{"--temperature", "2.1"}); err == nil {
		t.Fatal("ParseArgs accepted out-of-range temperature")
	}
}

func TestParseArgsYes(t *testing.T) {
	for _, flag := range []string{"-y", "--yes"} {
		args, err := ParseArgs([]string{flag, "--print", "hi"})
		if err != nil {
			t.Fatalf("ParseArgs(%q): %v", flag, err)
		}
		if !args.Yes {
			t.Fatalf("ParseArgs(%q): Yes = false", flag)
		}
		if args.Mode != ModePrint || args.Prompt != "hi" {
			t.Fatalf("ParseArgs(%q): Mode=%q Prompt=%q", flag, args.Mode, args.Prompt)
		}
	}
}

func TestParseArgsStatsRequiresPrintMode(t *testing.T) {
	args, err := ParseArgs([]string{"-p", "--stats", "stats.json", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if args.StatsPath != "stats.json" || args.Mode != ModePrint {
		t.Fatalf("StatsPath=%q Mode=%q", args.StatsPath, args.Mode)
	}

	if _, err := ParseArgs([]string{"--stats", "stats.json", "hi"}); err == nil {
		t.Fatal("ParseArgs accepted --stats without print mode")
	}
}

func TestParseArgsNoContextFiles(t *testing.T) {
	for _, flag := range []string{"--no-context-files", "-nc"} {
		args, err := ParseArgs([]string{flag})
		if err != nil {
			t.Fatalf("ParseArgs(%q): %v", flag, err)
		}
		if !args.NoContextFiles {
			t.Fatalf("ParseArgs(%q): NoContextFiles = false", flag)
		}
	}
}

func TestParseArgsStream(t *testing.T) {
	args, err := ParseArgs([]string{"--stream", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if args.Mode != ModeStream || args.Prompt != "hi" {
		t.Fatalf("Mode=%q Prompt=%q", args.Mode, args.Prompt)
	}
}
