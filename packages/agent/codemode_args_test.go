package agent

import "testing"

func TestCodemodeFlagsSelectLastMode(t *testing.T) {
	for _, tc := range []struct {
		flags []string
		mode  string
	}{
		{[]string{"--codemode"}, "on"},
		{[]string{"--codemode-only"}, "only"},
		{[]string{"--codemode-only", "--codemode"}, "on"},
		{[]string{"--codemode", "--codemode-only"}, "only"},
	} {
		args, err := ParseArgs(tc.flags)
		if err != nil || !args.Codemode || args.CodemodeMode != tc.mode {
			t.Fatalf("flags=%v mode=%s error=%v", tc.flags, args.CodemodeMode, err)
		}
	}
}
