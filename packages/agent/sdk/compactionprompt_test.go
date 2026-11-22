package sdk

import (
	"reflect"
	"testing"
)

func TestArgsFromConfigCompactionInstructions(t *testing.T) {
	want := []string{"Reference docs by path.", "Keep blockers."}
	args := argsFromConfig(Config{AppendCompactionPrompt: want})
	if !reflect.DeepEqual(args.AppendCompactionPrompt, want) {
		t.Fatalf("compaction additions = %v, want %v", args.AppendCompactionPrompt, want)
	}
}
