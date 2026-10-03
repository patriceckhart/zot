package codemode_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/codemode"
)

func TestMain(m *testing.M) {
	// Compile before tests start their script-specific deadlines.
	if err := codemode.Initialize(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestIsolatedVM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var items []codemode.Item
	result, err := codemode.Run(ctx, codemode.Start{Code: `text("hello"); return 2`}, func(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
		t.Error("unexpected host call")
		return nil, nil
	}, func(item codemode.Item) { items = append(items, item) })
	if err != nil || !result.OK || len(items) != 2 || items[0].Text != "hello" || items[1].Text != "2" {
		t.Fatalf("result=%+v items=%+v error=%v", result, items, err)
	}
}

func TestVMMemoryCapAndHardCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err := codemode.Run(ctx, codemode.Start{Code: `while(true) {}`}, nil, nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded script was not stopped by its deadline: %v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := codemode.Run(ctx, codemode.Start{Code: `return 1`}, nil, nil)
	if err != nil || !result.OK {
		t.Fatalf("a failed VM damaged subsequent executions: %v %+v", err, result)
	}
}

func TestVMMemoryLimitIndependentOfDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := codemode.Run(ctx, codemode.Start{Code: `return new ArrayBuffer(512*1024*1024)`}, nil, nil)
	if err == nil || ctx.Err() != nil || !strings.Contains(err.Error(), "memory limit") {
		t.Fatalf("allocation was not stopped by memory cap: %v, context=%v", err, ctx.Err())
	}
}
