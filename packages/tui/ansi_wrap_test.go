package tui

import (
	"reflect"
	"testing"
)

func TestWrapStyledANSILine(t *testing.T) {
	for _, tt := range []struct {
		name string
		text string
		want []string
	}{
		{
			name: "color across words",
			text: "\x1b[31maaaa bbbb cccc\x1b[0m",
			want: []string{"\x1b[31maaaa" + reset, "\x1b[31mbbbb" + reset, "\x1b[31mcccc" + reset + reset},
		},
		{
			name: "color across unbroken token",
			text: "\x1b[31maaaabbbb",
			want: []string{"\x1b[31maaaa" + reset, "\x1b[31mbbbb" + reset},
		},
		{
			name: "partial reset",
			text: "\x1b[31m\x1b[1maaaa \x1b[22mbbbb cccc",
			want: []string{"\x1b[31m\x1b[1maaaa" + reset, "\x1b[31m\x1b[1m\x1b[22mbbbb" + reset, "\x1b[31m\x1b[1m\x1b[22mcccc" + reset},
		},
		{
			name: "full reset",
			text: "\x1b[31maaaa\x1b[0m bbbb cccc",
			want: []string{"\x1b[31maaaa" + reset + reset, "bbbb" + reset, "cccc" + reset},
		},
		{
			name: "empty reset",
			text: "\x1b[31maaaa\x1b[m bbbb",
			want: []string{"\x1b[31maaaa\x1b[m" + reset, "bbbb" + reset},
		},
		{
			name: "extended colors containing zero",
			text: "\x1b[38;2;0;128;0m\x1b[48;5;0maaaa bbbb",
			want: []string{"\x1b[38;2;0;128;0m\x1b[48;5;0maaaa" + reset, "\x1b[38;2;0;128;0m\x1b[48;5;0mbbbb" + reset},
		},
		{
			name: "do not replay non-SGR controls",
			text: "\x1b[31m\x1b[2Kaaaa bbbb",
			want: []string{"\x1b[31m\x1b[2Kaaaa" + reset, "\x1b[31mbbbb" + reset},
		},
		{
			name: "short line",
			text: "\x1b[31mred",
			want: []string{"\x1b[31mred" + reset},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := WrapStyledANSILine(tt.text, 4); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("rows = %q, want %q", got, tt.want)
			}
		})
	}
}
