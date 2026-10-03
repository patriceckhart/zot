package codemode_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/patriceckhart/zot/packages/codemode"
)

func TestRunHostCapabilitiesAndState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initial := map[string]json.RawMessage{"value": json.RawMessage(`1`)}
	var items []codemode.Item
	result, err := codemode.Run(ctx, codemode.Start{
		Code:  `const [value, description] = await Promise.all([tools.my_tool({value:load("value")}), describeTool("my-tool")]); store("value",value.next); return description`,
		Tools: []codemode.Tool{{Name: "my-tool"}},
		Store: initial,
	}, func(_ context.Context, kind, name string, args json.RawMessage) (json.RawMessage, error) {
		switch {
		case kind == "tool" && name == "my-tool" && string(args) == `{"value":1}`:
			return json.RawMessage(`{"next":2}`), nil
		case kind == "global" && name == "describeTool" && string(args) == `["my-tool"]`:
			return json.RawMessage(`"host description"`), nil
		default:
			return nil, fmt.Errorf("unexpected capability %s/%s", kind, name)
		}
	}, func(item codemode.Item) { items = append(items, item) })
	if err != nil || !result.OK || !result.StoreWritten || string(result.Store["value"]) != "2" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if string(initial["value"]) != "1" || len(items) != 1 || items[0].Text != "host description" || codemode.Identifier("my-tool") != "my_tool" {
		t.Fatalf("initial=%v items=%+v", initial, items)
	}
}

func TestRunFailureDiscardsState(t *testing.T) {
	for _, code := range []string{
		`store("value",2); throw new Error("failed")`,
		`store("value",2); await searchTools("anything")`,
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result, err := codemode.Run(ctx, codemode.Start{Code: code}, nil, nil)
		cancel()
		if err != nil || result.OK || result.Error == "" || result.StoreWritten || len(result.Store) != 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

func TestRunCancelsAndWaitsForHostCalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan struct{})
	finished := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := codemode.Run(ctx, codemode.Start{Code: `await tools.wait({})`, Tools: []codemode.Tool{{Name: "wait"}}}, func(callCtx context.Context, _, _ string, _ json.RawMessage) (json.RawMessage, error) {
			close(started)
			<-callCtx.Done()
			defer close(finished)
			return nil, callCtx.Err()
		}, nil)
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("host callback did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled execution did not finish")
	}
	select {
	case <-finished:
	default:
		t.Fatal("execution returned before its host callback")
	}
}

func TestRunModelsRemainHostCapabilities(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := codemode.Run(ctx, codemode.Start{Code: `return await models.getModelsOfType("classifier")`, Models: true}, func(_ context.Context, kind, name string, args json.RawMessage) (json.RawMessage, error) {
		if kind != "global" || name != "models.getModelsOfType" || string(args) != `["classifier"]` {
			return nil, fmt.Errorf("unexpected model capability")
		}
		return nil, errors.New("synthetic host refusal")
	}, nil)
	if err != nil || result.OK || !strings.Contains(result.Error, "synthetic host refusal") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
