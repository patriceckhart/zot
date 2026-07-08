package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestToolArgWidthDefault(t *testing.T) {
	t.Setenv("ZOT_TOOL_ARG_WIDTH", "")
	if got := toolArgWidth(); got != defaultToolArgWidth {
		t.Fatalf("toolArgWidth() with unset env = %d, want %d", got, defaultToolArgWidth)
	}
}

func TestToolArgWidthEnvOverride(t *testing.T) {
	t.Setenv("ZOT_TOOL_ARG_WIDTH", "120")
	if got := toolArgWidth(); got != 120 {
		t.Fatalf("toolArgWidth() = %d, want 120", got)
	}
}

func TestToolArgWidthIgnoresInvalid(t *testing.T) {
	cases := []string{"nope", "0", "10", "501", "-5", "12.5"}
	for _, c := range cases {
		t.Setenv("ZOT_TOOL_ARG_WIDTH", c)
		if got := toolArgWidth(); got != defaultToolArgWidth {
			t.Fatalf("toolArgWidth() with %q = %d, want default %d", c, got, defaultToolArgWidth)
		}
	}
}

func TestShortArgsTruncatesAtDefaultWidth(t *testing.T) {
	t.Setenv("ZOT_TOOL_ARG_WIDTH", "")
	long := strings.Repeat("a", 200)
	raw := json.RawMessage(`{"command":"` + long + `"}`)
	got := ShortArgs("web_answer", raw)
	if len(got) != defaultToolArgWidth {
		t.Fatalf("ShortArgs length = %d, want %d (%q)", len(got), defaultToolArgWidth, got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("ShortArgs should end with ellipsis, got %q", got)
	}
}

func TestShortArgsRespectsWiderWidth(t *testing.T) {
	t.Setenv("ZOT_TOOL_ARG_WIDTH", "120")
	long := strings.Repeat("a", 200)
	raw := json.RawMessage(`{"command":"` + long + `"}`)
	got := ShortArgs("web_answer", raw)
	if len(got) != 120 {
		t.Fatalf("ShortArgs length = %d, want 120", len(got))
	}
}

func TestShortArgsNoTruncationWhenShort(t *testing.T) {
	t.Setenv("ZOT_TOOL_ARG_WIDTH", "120")
	raw := json.RawMessage(`{"command":"short query"}`)
	got := ShortArgs("web_answer", raw)
	if got != "short query" {
		t.Fatalf("ShortArgs = %q, want %q", got, "short query")
	}
}
