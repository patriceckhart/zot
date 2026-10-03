package codemode

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/patriceckhart/zot/packages/codemode/internal/vm"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

//go:generate go run ./internal/vm/generate -output worker.wasm.gz
//go:embed worker.wasm.gz
var workerAsset []byte

var compiledWorker struct {
	once    sync.Once
	runtime wazero.Runtime
	module  wazero.CompiledModule
	err     error
}

func workerModule() (wazero.Runtime, wazero.CompiledModule, error) {
	compiledWorker.once.Do(func() {
		reader, err := gzip.NewReader(bytes.NewReader(workerAsset))
		if err != nil {
			compiledWorker.err = err
			return
		}
		defer reader.Close()
		data, err := io.ReadAll(reader)
		if err != nil {
			compiledWorker.err = err
			return
		}
		ctx := context.Background()
		config := wazero.NewRuntimeConfig().WithMemoryLimitPages(4096).WithCloseOnContextDone(true)
		runtime := wazero.NewRuntimeWithConfig(ctx, config)
		if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
			compiledWorker.err = err
			_ = runtime.Close(ctx)
			return
		}
		module, err := runtime.CompileModule(ctx, data)
		if err != nil {
			compiledWorker.err = err
			_ = runtime.Close(ctx)
			return
		}
		compiledWorker.runtime, compiledWorker.module = runtime, module
	})
	return compiledWorker.runtime, compiledWorker.module, compiledWorker.err
}

// runVM gives each execution a fresh, capped linear memory and no mounted
// filesystem. Context cancellation closes both the VM and blocked protocol I/O.
func runVM(ctx context.Context, start Start, call Caller, emit func(Item)) (codemodevm.Frame, error) {
	runtime, module, err := workerModule()
	if err != nil {
		return codemodevm.Frame{}, fmt.Errorf("initialize JavaScript VM: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	initial, err := json.Marshal(start)
	if err != nil {
		return codemodevm.Frame{}, err
	}
	initial = append(initial, '\n')
	inputR, inputW := io.Pipe()
	outputR, outputW := io.Pipe()
	defer inputR.Close()
	defer inputW.Close()
	defer outputR.Close()
	defer outputW.Close()
	stop := context.AfterFunc(ctx, func() {
		_ = inputR.CloseWithError(ctx.Err())
		_ = inputW.CloseWithError(ctx.Err())
		_ = outputR.CloseWithError(ctx.Err())
		_ = outputW.CloseWithError(ctx.Err())
	})
	defer stop()
	finished := make(chan error, 1)
	go func() {
		config := wazero.NewModuleConfig().WithName("").WithStdin(io.MultiReader(bytes.NewReader(initial), inputR)).WithStdout(outputW).WithStderr(io.Discard).WithRandSource(rand.Reader).WithSysWalltime().WithSysNanotime()
		_, err := runtime.InstantiateModule(ctx, module, config)
		_ = outputW.Close()
		finished <- err
	}()
	var calls sync.WaitGroup
	defer func() {
		cancel()
		<-finished
		calls.Wait()
	}()
	var replies sync.Mutex
	decoder := json.NewDecoder(outputR)
	for {
		var frame codemodevm.Frame
		if err := decoder.Decode(&frame); err != nil {
			if ctx.Err() != nil {
				return frame, ctx.Err()
			}
			return frame, fmt.Errorf("JavaScript VM stopped before completion (memory limit or worker failure)")
		}
		switch frame.Type {
		case "output":
			if frame.Item == nil {
				return frame, fmt.Errorf("invalid JavaScript output frame")
			}
			emit(*frame.Item)
		case "done":
			return frame, nil
		case "tool", "global":
			calls.Add(1)
			go func() {
				defer calls.Done()
				var value json.RawMessage
				var err error
				func() {
					defer func() {
						if recover() != nil {
							err = fmt.Errorf("panic in codemode host capability")
						}
					}()
					if call == nil {
						err = fmt.Errorf("host capabilities are unavailable")
						return
					}
					value, err = call(ctx, frame.Type, frame.Name, frame.Args)
				}()
				if len(value) > 0 && !json.Valid(value) {
					err = fmt.Errorf("capability returned invalid JSON")
					value = nil
				}
				reply := codemodevm.Frame{Type: "reply", ID: frame.ID, OK: err == nil, Value: value}
				if err != nil {
					reply.Error = err.Error()
				}
				replies.Lock()
				defer replies.Unlock()
				if ctx.Err() == nil {
					_ = json.NewEncoder(inputW).Encode(reply)
				}
			}()
		default:
			return frame, fmt.Errorf("invalid JavaScript worker frame")
		}
	}
}
