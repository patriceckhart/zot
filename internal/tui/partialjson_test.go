package tui

import "testing"

func TestExtractLastNewText(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantValue string
		wantOK    bool
		wantDone  bool
		wantIdx   int
	}{
		{
			name:      "no newText yet",
			raw:       `{"path":"/x","edits":[{"oldText":"a"}`,
			wantValue: "",
			wantOK:    false,
			wantDone:  false,
			wantIdx:   0,
		},
		{
			name:      "single newText partial",
			raw:       `{"edits":[{"oldText":"a","newText":"b`,
			wantValue: "b",
			wantOK:    true,
			wantDone:  false,
			wantIdx:   1,
		},
		{
			name:      "single newText complete",
			raw:       `{"edits":[{"oldText":"a","newText":"b"}]`,
			wantValue: "b",
			wantOK:    true,
			wantDone:  true,
			wantIdx:   1,
		},
		{
			name:      "two edits, second still streaming",
			raw:       `{"edits":[{"oldText":"x","newText":"y"},{"oldText":"a","newText":"hello wor`,
			wantValue: "hello wor",
			wantOK:    true,
			wantDone:  false,
			wantIdx:   2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok, done, idx := ExtractLastNewText(c.raw)
			if v != c.wantValue || ok != c.wantOK || done != c.wantDone || idx != c.wantIdx {
				t.Errorf("want (v=%q ok=%v done=%v idx=%d), got (v=%q ok=%v done=%v idx=%d)",
					c.wantValue, c.wantOK, c.wantDone, c.wantIdx, v, ok, done, idx)
			}
		})
	}
}

func TestExtractPartialStringField(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		field     string
		wantValue string
		wantOK    bool
		wantDone  bool
	}{
		{
			name:      "empty buffer",
			raw:       "",
			field:     "content",
			wantValue: "",
			wantOK:    false,
			wantDone:  false,
		},
		{
			name:      "no such field",
			raw:       `{"path":"/x","foo":"bar"}`,
			field:     "content",
			wantValue: "",
			wantOK:    false,
			wantDone:  false,
		},
		{
			name:      "complete",
			raw:       `{"path":"/x","content":"hello"}`,
			field:     "content",
			wantValue: "hello",
			wantOK:    true,
			wantDone:  true,
		},
		{
			name:      "partial mid-word",
			raw:       `{"path":"/x","content":"hel`,
			field:     "content",
			wantValue: "hel",
			wantOK:    true,
			wantDone:  false,
		},
		{
			name:      "escaped quote",
			raw:       `{"content":"say \"hi\""}`,
			field:     "content",
			wantValue: `say "hi"`,
			wantOK:    true,
			wantDone:  true,
		},
		{
			name:      "escaped newline inside string",
			raw:       `{"content":"line1\nline2"}`,
			field:     "content",
			wantValue: "line1\nline2",
			wantOK:    true,
			wantDone:  true,
		},
		{
			name:      "trailing backslash (unfinished escape)",
			raw:       `{"content":"line1\`,
			field:     "content",
			wantValue: "line1",
			wantOK:    true,
			wantDone:  false,
		},
		{
			name:      "incomplete unicode escape",
			raw:       `{"content":"before\u00`,
			field:     "content",
			wantValue: "before",
			wantOK:    true,
			wantDone:  false,
		},
		{
			name:      "key before value",
			raw:       `{"content":`,
			field:     "content",
			wantValue: "",
			wantOK:    false,
			wantDone:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v, ok, done := ExtractPartialStringField(c.raw, c.field)
			if v != c.wantValue {
				t.Errorf("value: want %q, got %q", c.wantValue, v)
			}
			if ok != c.wantOK {
				t.Errorf("ok: want %v, got %v", c.wantOK, ok)
			}
			if done != c.wantDone {
				t.Errorf("done: want %v, got %v", c.wantDone, done)
			}
		})
	}
}
