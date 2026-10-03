package worker

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/codemode/internal/vm"
)

func runScript(t *testing.T, code string, tools []codemodevm.Tool, replies ...codemodevm.Frame) []codemodevm.Frame {
	t.Helper()
	var input, output bytes.Buffer
	encoder := json.NewEncoder(&input)
	if err := encoder.Encode(codemodevm.Start{Code: code, Tools: tools, Store: map[string]json.RawMessage{}}); err != nil {
		t.Fatal(err)
	}
	for _, reply := range replies {
		if err := encoder.Encode(reply); err != nil {
			t.Fatal(err)
		}
	}
	if err := Serve(&input, &output); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&output)
	var frames []codemodevm.Frame
	for decoder.More() {
		var frame codemodevm.Frame
		if err := decoder.Decode(&frame); err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func TestGlobalsAreImmutableAndGuardMissingMembers(t *testing.T) {
	frames := runScript(t, `text(Object.isFrozen(tools)); text(Object.isFrozen(ALL_TOOLS)); text(Object.isFrozen(ALL_TOOLS[0])); text(Object.isFrozen(console)); text("missing" in tools); try { tools.MyTool } catch (e) { text(e.message) }`, []codemodevm.Tool{{Name: "my-tool", Description: "example"}})
	want := []string{"true", "true", "true", "true", "false"}
	for i, text := range want {
		if frames[i].Item.Text != text {
			t.Fatalf("frame %d: %+v", i, frames[i])
		}
	}
	if message := frames[5].Item.Text; !strings.Contains(message, "Did you mean tools.my_tool") || !strings.Contains(message, "ALL_TOOLS") {
		t.Fatal(message)
	}
	if !frames[len(frames)-1].OK {
		t.Fatal(frames)
	}
}

func TestToolSerializationFailuresRejectPromises(t *testing.T) {
	frames := runScript(t, `const circular = {}; circular.self = circular; const settled = await Promise.allSettled([tools.echo(circular),tools.echo(null)]); return settled.map(item => item.status)`, []codemodevm.Tool{{Name: "echo"}})
	if len(frames) != 2 || frames[0].Item.Text != `["rejected","rejected"]` || !frames[1].OK {
		t.Fatalf("frames: %+v", frames)
	}
}

func TestIdentifierCollisionKeepsFirstTool(t *testing.T) {
	frames := runScript(t, `text(ALL_TOOLS.map(tool => tool.name)); return await tools.my_tool({})`, []codemodevm.Tool{{Name: "my-tool"}, {Name: "my_tool"}}, codemodevm.Frame{ID: 1, OK: true, Value: json.RawMessage(`"first"`)})
	if frames[0].Item.Text != `["my_tool"]` || frames[1].Name != "my-tool" || frames[2].Item.Text != "first" {
		t.Fatalf("frames: %+v", frames)
	}
}

func TestConsoleFormattingAndSerializableReturn(t *testing.T) {
	frames := runScript(t, `const circular = {}; circular.self = circular; console.log(circular); console.error(new Error("broken")); text(3n); return () => 1`, nil)
	if !frames[len(frames)-1].OK || len(frames) != 4 || frames[0].Item.Text != "[object Object]" || !strings.Contains(frames[1].Item.Text, "Error: broken") || frames[2].Item.Text != "3" {
		t.Fatalf("frames: %+v", frames)
	}
	frames = runScript(t, `return 3n`, nil)
	if frames[len(frames)-1].OK || !strings.Contains(frames[len(frames)-1].Error, "BigInt") {
		t.Fatalf("frames: %+v", frames)
	}
}

func TestStoreCountsKeysAndUsesRangeError(t *testing.T) {
	frames := runScript(t, `try { store("large", "x".repeat(262143)); } catch (e) { text(e instanceof RangeError); } try { store("x".repeat(1048576), 1); } catch (e) { text(e instanceof RangeError); } store("absent",undefined)`, nil)
	if len(frames) != 3 || frames[0].Item.Text != "true" || frames[1].Item.Text != "true" || !frames[2].OK || !frames[2].StoreWritten {
		t.Fatalf("frames: %+v", frames)
	}
}

func TestErrorStackIncludesScriptLocation(t *testing.T) {
	frames := runScript(t, "\nthrow new Error('broken')", nil)
	failure := frames[len(frames)-1]
	if failure.OK || !strings.Contains(failure.Error, "Error: broken") || !strings.Contains(failure.Error, "codemode.js:2") {
		t.Fatalf("failure: %+v", failure)
	}
}
