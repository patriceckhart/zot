package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShellStructuredOutputAndCompleteSpill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	cwd := t.TempDir()
	output := strings.Repeat("x", 2*maxScriptBashBytes) + "END"
	if err := os.WriteFile(filepath.Join(cwd, "output.txt"), []byte(output), 0600); err != nil {
		t.Fatal(err)
	}
	tool := &BashTool{CWD: cwd}
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"command":"cat output.txt"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Output    string  `json:"output"`
		Truncated bool    `json:"truncated"`
		Path      string  `json:"full_output_path"`
		Exit      int     `json:"exit_code"`
		Seconds   float64 `json:"wall_time_seconds"`
	}
	if err := json.Unmarshal(res.StructuredContent, &value); err != nil {
		t.Fatal(err)
	}
	marker := fmt.Sprintf("\n\n[... %d bytes omitted ...]\n\n", len(output)-maxScriptBashBytes)
	wantOutput := output[:maxScriptBashBytes/2] + marker + output[len(output)-maxScriptBashBytes/2:]
	if res.IsError || value.Output != wantOutput || !value.Truncated || value.Exit != 0 || value.Seconds <= 0 || value.Path == "" {
		t.Fatalf("unexpected structured result: size=%d truncated=%v exit=%d path=%q", len(value.Output), value.Truncated, value.Exit, value.Path)
	}
	t.Cleanup(func() { os.Remove(value.Path) })
	full, err := os.ReadFile(value.Path)
	if err != nil || string(full) != output {
		t.Fatalf("spill is incomplete: size=%d err=%v", len(full), err)
	}
	info, err := os.Stat(value.Path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("spill permissions: %v %v", info, err)
	}
}

func TestShellStructuredSmallAndFailedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	for _, exit := range []int{0, 7} {
		raw, _ := json.Marshal(map[string]string{"command": fmt.Sprintf("printf hello\nexit %d", exit)})
		res, err := (&BashTool{CWD: t.TempDir()}).Execute(context.Background(), raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		var value map[string]any
		if err := json.Unmarshal(res.StructuredContent, &value); err != nil {
			t.Fatal(err)
		}
		if value["output"] != "hello" || value["exit_code"] != float64(exit) || value["truncated"] != false || res.IsError != (exit != 0) {
			t.Fatalf("%+v", value)
		}
		if _, ok := value["full_output_path"]; ok {
			t.Fatal("small output should not leave a spill file")
		}
	}
}
