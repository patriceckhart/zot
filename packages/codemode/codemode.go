// Package codemode executes JavaScript in an embedded, memory-capped
// WebAssembly VM. Outside effects are provided through explicit host callbacks.
// It has no dependency on agent tools, providers, credentials or sessions.
package codemode

import (
	"context"
	"encoding/json"

	"github.com/patriceckhart/zot/packages/codemode/internal/vm"
)

// Tool describes a host capability exposed through the script's tools object.
type Tool = codemodevm.Tool

// Start configures one invocation. Code is an async JavaScript function body.
// Store supplies JSON state, and Models enables the host-backed models API.
// Each invocation starts a fresh VM, regardless of the supplied state.
type Start = codemodevm.Start

// Item is emitted text or a base64 image, delivered in script output order.
type Item = codemodevm.Item

// Caller handles a host capability request. Kind is "tool" or "global".
// Global requests include discovery and, when enabled, model operations.
// Requests may run concurrently. Implementations must honor cancellation.
// A nil JSON value resolves to JavaScript undefined.
type Caller func(ctx context.Context, kind, name string, args json.RawMessage) (json.RawMessage, error)

// Result contains script completion and its successful JSON state snapshot.
// Script exceptions set OK to false and populate Error without a Go error.
// StoreWritten indicates that the script changed state. The caller owns
// persistence, and must discard state if execution fails or is cancelled.
type Result struct {
	OK           bool
	Error        string
	Store        map[string]json.RawMessage
	StoreWritten bool
}

// Initialize compiles the shared worker once. Call it before starting a
// script-specific deadline if compilation should not count against that deadline.
func Initialize() error {
	_, _, err := workerModule()
	return err
}

// Run executes a script with a fresh 256 MiB linear-memory limit and no mounted
// filesystem. Context cancellation stops the VM and cancels host requests.
// Run waits for host requests to return before completing, so a callback that
// ignores cancellation can delay cleanup. Output callbacks run serially.
// A nil emit discards output, and a nil call disables host capabilities.
func Run(ctx context.Context, start Start, call Caller, emit func(Item)) (Result, error) {
	if emit == nil {
		emit = func(Item) {}
	}
	frame, err := runVM(ctx, start, call, emit)
	return Result{OK: frame.OK, Error: frame.Error, Store: frame.Store, StoreWritten: frame.StoreWritten}, err
}

// Identifier preserves ASCII identifier characters, replacing everything else.
// It uses the same normalization as the worker's tools object.
func Identifier(name string) string {
	return codemodevm.Identifier(name)
}
