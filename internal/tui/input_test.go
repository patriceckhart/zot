package tui

import "testing"

func TestReaderParsesSGRMouseWheel(t *testing.T) {
	cases := []struct {
		seq  string
		want KeyKind
	}{
		{"\x1b[<64;10;20M", KeyMouseWheelUp},
		{"\x1b[<65;10;20M", KeyMouseWheelDown},
	}
	for _, tc := range cases {
		idx := 0
		r := NewReader(func() (byte, error) {
			b := tc.seq[idx]
			idx++
			return b, nil
		})
		k, err := r.Read()
		if err != nil {
			t.Fatalf("Read(%q): %v", tc.seq, err)
		}
		if k.Kind != tc.want {
			t.Fatalf("Read(%q) kind=%v, want %v", tc.seq, k.Kind, tc.want)
		}
	}
}
