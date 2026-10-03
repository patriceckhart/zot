package worker

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/codemode/internal/vm"
)

// Decode each frame as it is written so boundary tests do not retain large output.
type outputSummary struct {
	items int
	chars int
	done  codemodevm.Frame
}

func (s *outputSummary) Write(data []byte) (int, error) {
	var frame codemodevm.Frame
	if err := json.Unmarshal(data, &frame); err != nil {
		return 0, err
	}
	if frame.Type == "output" {
		s.items++
		value := frame.Item.Text
		if frame.Item.Type == "image" {
			value = frame.Item.Data
		}
		for _, r := range value {
			s.chars++
			if r > 0xffff {
				s.chars++
			}
		}
	} else if frame.Type == "done" {
		s.done = frame
	}
	return len(data), nil
}

func TestOutputLimits(t *testing.T) {
	for _, tc := range []struct {
		name, code   string
		items, chars int
		ok           bool
	}{
		{"character boundary", `const chunk = "x".repeat(65536); for (let i = 0; i < 256; i++) text(chunk)`, 256, 16777216, true},
		{"character overflow", `const chunk = "x".repeat(65536); for (let i = 0; i < 256; i++) text(chunk); text("x")`, 256, 16777216, false},
		{"UTF-16 overflow", `const chunk = "\u{1f600}".repeat(32768); for (let i = 0; i < 256; i++) text(chunk); text("x")`, 256, 16777216, false},
		{"mixed output overflow", `const chunk = "x".repeat(65536); for (let i = 0; i < 255; i++) console.log(chunk); text("x".repeat(65524)); image("data:image/png;base64,iVBORw0KGgo="); text("x")`, 257, 16777216, false},
		{"image overflow", `const chunk = "x".repeat(65536); for (let i = 0; i < 255; i++) text(chunk); text("x".repeat(65525)); image("data:image/png;base64,iVBORw0KGgo=")`, 256, 16777205, false},
		{"return overflow", `store("value",1); const chunk = "x".repeat(65536); for (let i = 0; i < 256; i++) text(chunk); return "x"`, 256, 16777216, false},
		{"item boundary", `for (let i = 0; i < 99998; i++) text(""); console.log(); image("data:image/png;base64,iVBORw0KGgo=")`, 100000, 12, true},
		{"console item overflow", `for (let i = 0; i < 100000; i++) text(""); console.log()`, 100000, 0, false},
		{"item overflow cannot be caught", `store("value",1); try { for (let i = 0; i < 100001; i++) text("") } catch (e) {} finally { exit() }`, 100000, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var input bytes.Buffer
			if err := json.NewEncoder(&input).Encode(codemodevm.Start{Code: tc.code}); err != nil {
				t.Fatal(err)
			}
			var summary outputSummary
			if err := Serve(&input, &summary); err != nil {
				t.Fatal(err)
			}
			if summary.done.Type != "done" || summary.done.OK != tc.ok || summary.items != tc.items || summary.chars != tc.chars {
				t.Fatalf("completion=%+v items=%d chars=%d", summary.done, summary.items, summary.chars)
			}
			if !tc.ok && (!strings.Contains(summary.done.Error, "script output exceeded the limit") || summary.done.StoreWritten || len(summary.done.Store) != 0) {
				t.Fatalf("failure did not discard state: %+v", summary.done)
			}
		})
	}
}
