package codemode

import (
	"fmt"
	"os"
	"testing"

	executor "github.com/patriceckhart/zot/packages/codemode"
)

func TestMain(m *testing.M) {
	// Compilation is shared initialization, not part of individual script
	// deadlines. Race-instrumented compilation can take considerably longer.
	if err := executor.Initialize(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
