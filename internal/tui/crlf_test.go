package tui

import "testing"

func TestPasteNormalizesCarriageReturns(t *testing.T) {
	e := NewEditor("▌ ")
	e.HandleKey(Key{Kind: KeyPaste, Paste: "alpha\rbeta\r\ngamma"})

	want := "alpha\nbeta\ngamma"
	if got := e.Value(); got != want {
		t.Fatalf("Value() = %q, want %q", got, want)
	}
	if got := e.SubmitValue(); got != want {
		t.Fatalf("SubmitValue() = %q, want %q", got, want)
	}
}

func TestSetValueNormalizesCarriageReturns(t *testing.T) {
	e := NewEditor("▌ ")
	e.SetValue("one\rtwo\r\nthree")

	want := "one\ntwo\nthree"
	if got := e.Value(); got != want {
		t.Fatalf("Value() = %q, want %q", got, want)
	}
}
