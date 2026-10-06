package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

// Streamed progress of a running call is shown in its panel, in box, flat,
// and collapsed layouts, and a result replaces it.
func TestToolProgressRendersUntilResult(t *testing.T) {
	args := json.RawMessage(`{"command":"make test"}`)
	for _, v := range []View{
		{Theme: Dark},
		{Theme: Dark, FlatTools: true},
		{Theme: Dark, CollapseToolCall: true},
	} {
		v.ToolCalls = []ToolCallView{{ID: "c1", Name: "bash", Args: ShortArgs("bash", args), RawJSONBuf: string(args), Progress: "compiling pkg\n"}}
		plain := stripANSI(strings.Join(v.Build(80), "\n"))
		if !strings.Contains(plain, "compiling pkg") {
			t.Fatalf("progress not rendered (flat=%v collapsed=%v):\n%s", v.FlatTools, v.CollapseToolCall, plain)
		}
		v.ToolCalls[0].Result, v.ToolCalls[0].Progress, v.ToolCalls[0].Done = "ok\n", "", true
		plain = stripANSI(strings.Join(v.Build(80), "\n"))
		if strings.Contains(plain, "compiling pkg") || !strings.Contains(plain, "ok") {
			t.Fatalf("result did not replace progress:\n%s", plain)
		}
	}
}
