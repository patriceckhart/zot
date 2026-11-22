package tui

import (
	"strings"
	"testing"
)

func TestEditorMaskPastesLiteralSecrets(t *testing.T) {
	for _, text := range []string{"/synthetic-secret/key", "~/synthetic-secret", "file:///synthetic-secret", "first\nsecond\nthird"} {
		ed := NewEditor("")
		ed.Mask = true
		ed.HandleKey(Key{Kind: KeyPaste, Paste: text})
		if ed.SubmitValue() != text || ed.Value() != text {
			t.Fatal("masked paste transformed the secret")
		}
		if len(ed.SubmissionFiles()) != 0 || len(ed.pastes) != 0 {
			t.Fatal("masked paste created an attachment or placeholder")
		}
	}
}

func TestEditorMaskConcealsRenderedValue(t *testing.T) {
	ed := NewEditor("")
	ed.SetValue("secret-key")
	ed.Mask = true
	lines, _, _ := ed.Render(80)
	rendered := strings.Join(lines, "\n")
	if strings.Contains(rendered, "secret-key") || rendered != "**********" {
		t.Fatalf("rendered = %q", rendered)
	}
	if got := ed.SubmitValue(); got != "secret-key" {
		t.Fatalf("submitted = %q", got)
	}
}
