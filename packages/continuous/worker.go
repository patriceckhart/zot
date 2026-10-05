package continuous

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

// Remote execution environment.
//
// A worker runs tools in another process or on another machine. Every call
// carries a stable operation key, the task (run) identity, the writer epoch,
// and the environment identity. The worker records each operation it starts
// so the host can look it up after a connection loss instead of assuming the
// process stopped. The transport is the same newline-JSON framing as the host
// protocol; remote workers must run behind TLS and a token. This is a
// reference implementation of the contract, not a cluster scheduler.

// WorkerRequest is one frame from host to worker.
type WorkerRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// WorkerResponse is one frame from worker to host.
type WorkerResponse struct {
	ID      string          `json:"id"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   string          `json:"error,omitempty"`
}

// WorkerCall is the payload of tool.execute.
type WorkerCall struct {
	OperationKey   string          `json:"operation_key"`
	RunID          string          `json:"run_id"`
	ConversationID string          `json:"conversation_id"`
	CallID         string          `json:"call_id"`
	Epoch          uint64          `json:"epoch"`
	Environment    string          `json:"environment"`
	Tool           string          `json:"tool"`
	Args           json.RawMessage `json:"args"`
}

// WorkerResult is the payload of a completed tool.execute or a lookup hit.
type WorkerResult struct {
	Content []provider.Content `json:"-"`
	Blocks  []json.RawMessage  `json:"content"`
	IsError bool               `json:"is_error"`
	Status  string             `json:"status,omitempty"`
}

// WorkerLookup is the payload of tool.lookup.
type WorkerLookup struct {
	OperationKey string `json:"operation_key"`
}

// WorkerLookupResult reports what the worker knows about an operation.
type WorkerLookupResult struct {
	// State is completed, running, not_started, or unknown.
	State  string        `json:"state"`
	Result *WorkerResult `json:"result,omitempty"`
}

// WorkerHello describes the worker and its capabilities.
type WorkerHello struct {
	Environment string   `json:"environment"`
	Tools       []string `json:"tools"`
	// Lookup is true when the worker retains operations and answers
	// tool.lookup. Without it, every interrupted remote call is unknown.
	Lookup bool `json:"lookup"`
}

// WorkerServer executes tools from a registry on behalf of hosts and keeps an
// in-memory operation ledger for lookup and deduplication. Operations are
// retained for Retention after they complete (default one hour).
type WorkerServer struct {
	Environment string
	Tools       core.Registry
	Token       string
	Retention   time.Duration
	// LedgerPath, when set, persists started and completed operations as
	// append-only JSON lines so a worker restart still answers lookups.
	// Operations that were running at the crash are reported unknown: the
	// worker cannot tell whether the effect happened.
	LedgerPath string

	mu     sync.Mutex
	ops    map[string]*workerOp
	epochs map[string]uint64
	ledger *os.File
}

// workerLedgerLine is one persisted ledger event.
type workerLedgerLine struct {
	Key     string        `json:"key"`
	State   string        `json:"state"`
	Epoch   uint64        `json:"epoch,omitempty"`
	Conv    string        `json:"conversation,omitempty"`
	Result  *WorkerResult `json:"result,omitempty"`
	Expires time.Time     `json:"expires,omitzero"`
}

// Open loads the ledger when LedgerPath is set. Serve and ServeConn call it
// lazily; call it directly to surface load errors early.
func (w *WorkerServer) Open() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.initLocked()
}

func (w *WorkerServer) initLocked() error {
	if w.ops != nil {
		return nil
	}
	w.ops = map[string]*workerOp{}
	w.epochs = map[string]uint64{}
	if w.Retention <= 0 {
		w.Retention = time.Hour
	}
	if w.LedgerPath == "" {
		return nil
	}
	f, err := os.OpenFile(w.LedgerPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 16<<20)
	for scanner.Scan() {
		var line workerLedgerLine
		if json.Unmarshal(scanner.Bytes(), &line) != nil || line.Key == "" {
			continue
		}
		switch line.State {
		case "running":
			// Started before a crash: the effect may or may not have happened.
			done := make(chan struct{})
			close(done)
			w.ops[line.Key] = &workerOp{state: "unknown", done: done}
			if line.Epoch > w.epochs[line.Conv] {
				w.epochs[line.Conv] = line.Epoch
			}
		case "completed":
			done := make(chan struct{})
			close(done)
			w.ops[line.Key] = &workerOp{state: "completed", result: line.Result, done: done, expires: line.Expires}
		}
	}
	if err := scanner.Err(); err != nil {
		f.Close()
		return err
	}
	w.ledger = f
	return nil
}

func (w *WorkerServer) appendLedger(line workerLedgerLine) {
	if w.ledger == nil {
		return
	}
	b, _ := json.Marshal(line)
	w.ledger.Write(append(b, '\n'))
	w.ledger.Sync()
}

// Close releases the ledger file.
func (w *WorkerServer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ledger != nil {
		err := w.ledger.Close()
		w.ledger = nil
		return err
	}
	return nil
}

type workerOp struct {
	state   string
	result  *WorkerResult
	done    chan struct{}
	expires time.Time
}

// Serve accepts connections until ctx ends.
func (w *WorkerServer) Serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		wg.Add(1)
		go func() { defer wg.Done(); w.ServeConn(ctx, conn) }()
	}
}

// ServeConn serves one host connection.
func (w *WorkerServer) ServeConn(ctx context.Context, conn io.ReadWriteCloser) {
	w.mu.Lock()
	initErr := w.initLocked()
	w.mu.Unlock()
	defer conn.Close()
	if initErr != nil {
		b, _ := json.Marshal(WorkerResponse{Error: "worker ledger unavailable: " + initErr.Error()})
		conn.Write(append(b, '\n'))
		return
	}
	var writeMu sync.Mutex
	write := func(resp WorkerResponse) {
		b, _ := json.Marshal(resp)
		writeMu.Lock()
		conn.Write(append(b, '\n'))
		writeMu.Unlock()
	}
	fail := func(id, msg string) { write(WorkerResponse{ID: id, Error: msg}) }
	authed := w.Token == ""
	reader := bufio.NewReaderSize(conn, 1<<20)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { <-ctx.Done(); conn.Close() }()
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var req WorkerRequest
		if json.Unmarshal(line, &req) != nil {
			fail("", "malformed request")
			continue
		}
		if !authed {
			var p struct {
				Token string `json:"token"`
			}
			if req.Method != "hello" || json.Unmarshal(req.Params, &p) != nil || p.Token != w.Token {
				fail(req.ID, "unauthorized")
				return
			}
			authed = true
		}
		switch req.Method {
		case "hello":
			names := make([]string, 0, len(w.Tools))
			for name := range w.Tools {
				names = append(names, name)
			}
			data, _ := json.Marshal(WorkerHello{Environment: w.Environment, Tools: names, Lookup: true})
			write(WorkerResponse{ID: req.ID, Success: true, Data: data})
		case "tool.lookup":
			var p WorkerLookup
			if json.Unmarshal(req.Params, &p) != nil || p.OperationKey == "" {
				fail(req.ID, "operation_key required")
				continue
			}
			data, _ := json.Marshal(w.lookup(p.OperationKey))
			write(WorkerResponse{ID: req.ID, Success: true, Data: data})
		case "tool.execute":
			var call WorkerCall
			if json.Unmarshal(req.Params, &call) != nil || call.OperationKey == "" || call.Tool == "" {
				fail(req.ID, "invalid call")
				continue
			}
			go func() {
				result, err := w.execute(ctx, call)
				if err != nil {
					fail(req.ID, err.Error())
					return
				}
				data, _ := json.Marshal(result)
				write(WorkerResponse{ID: req.ID, Success: true, Data: data})
			}()
		default:
			fail(req.ID, "unknown method "+req.Method)
		}
	}
}

func (w *WorkerServer) lookup(key string) WorkerLookupResult {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sweep()
	op, ok := w.ops[key]
	if !ok {
		return WorkerLookupResult{State: "not_started"}
	}
	if op.state == "running" {
		// Still executing in this process: the host should ask again, not
		// treat it as done or as never started.
		return WorkerLookupResult{State: "unknown"}
	}
	return WorkerLookupResult{State: op.state, Result: op.result}
}

func (w *WorkerServer) sweep() {
	now := time.Now()
	for key, op := range w.ops {
		if op.state == "completed" && now.After(op.expires) {
			delete(w.ops, key)
		}
	}
}

// execute runs a call once per operation key. A repeated key waits for and
// returns the original result; the tool never runs twice for one key.
func (w *WorkerServer) execute(ctx context.Context, call WorkerCall) (*WorkerResult, error) {
	w.mu.Lock()
	w.sweep()
	if newest := w.epochs[call.ConversationID]; call.Epoch < newest {
		w.mu.Unlock()
		return nil, fmt.Errorf("stale writer epoch %d (newest %d)", call.Epoch, newest)
	}
	w.epochs[call.ConversationID] = call.Epoch
	if op, ok := w.ops[call.OperationKey]; ok {
		w.mu.Unlock()
		select {
		case <-op.done:
			if op.state == "unknown" {
				return nil, fmt.Errorf("operation %s was running when the worker restarted; its outcome is unknown", call.OperationKey)
			}
			return op.result, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	op := &workerOp{state: "running", done: make(chan struct{})}
	w.ops[call.OperationKey] = op
	w.appendLedger(workerLedgerLine{Key: call.OperationKey, State: "running", Epoch: call.Epoch, Conv: call.ConversationID})
	w.mu.Unlock()
	tool, err := w.Tools.Get(call.Tool)
	var result WorkerResult
	if err != nil {
		result = WorkerResult{IsError: true, Status: "failed", Blocks: encodeBlocks([]provider.Content{provider.TextBlock{Text: err.Error()}})}
	} else {
		// Detached from the connection: a dropped host does not stop the
		// effect, which is exactly why lookup exists.
		execCtx := core.WithToolOperationKey(context.WithoutCancel(ctx), call.OperationKey)
		res, err := tool.Execute(execCtx, call.Args, func(string) {})
		if err != nil {
			result = WorkerResult{IsError: true, Status: "failed", Blocks: encodeBlocks([]provider.Content{provider.TextBlock{Text: err.Error()}})}
		} else {
			result = WorkerResult{IsError: res.IsError, Status: res.Status, Blocks: encodeBlocks(res.Content)}
		}
	}
	w.mu.Lock()
	op.state, op.result, op.expires = "completed", &result, time.Now().Add(w.Retention)
	w.appendLedger(workerLedgerLine{Key: call.OperationKey, State: "completed", Result: &result, Expires: op.expires})
	close(op.done)
	w.mu.Unlock()
	return &result, nil
}

func encodeBlocks(content []provider.Content) []json.RawMessage {
	msg := provider.Message{Role: provider.RoleTool, Content: content}
	b, _ := json.Marshal(msg)
	var decoded struct {
		Content []json.RawMessage `json:"content"`
	}
	json.Unmarshal(b, &decoded)
	return decoded.Content
}

func decodeBlocks(blocks []json.RawMessage) []provider.Content {
	raw, _ := json.Marshal(map[string]any{"role": "tool", "content": blocks})
	msg, err := core.DecodeMessage(raw)
	if err != nil {
		return []provider.Content{provider.TextBlock{Text: "undecodable remote result"}}
	}
	return msg.Content
}

// WorkerClient talks to one worker.
type WorkerClient struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
	next   int
	calls  map[string]chan WorkerResponse
	Hello  WorkerHello
}

// DialWorker connects and performs the hello handshake. tlsConfig is required
// for non-loopback addresses by policy; this function does not enforce
// network topology and the caller must.
func DialWorker(ctx context.Context, conn net.Conn, token string) (*WorkerClient, error) {
	c := &WorkerClient{conn: conn, reader: bufio.NewReaderSize(conn, 1<<20), calls: map[string]chan WorkerResponse{}}
	go c.readLoop()
	data, err := c.call(ctx, "hello", map[string]any{"token": token})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := json.Unmarshal(data, &c.Hello); err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *WorkerClient) Close() error { return c.conn.Close() }

func (c *WorkerClient) readLoop() {
	defer func() {
		c.mu.Lock()
		for id, ch := range c.calls {
			close(ch)
			delete(c.calls, id)
		}
		c.mu.Unlock()
	}()
	for {
		line, err := c.reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var resp WorkerResponse
		if json.Unmarshal(line, &resp) != nil {
			continue
		}
		c.mu.Lock()
		if ch, ok := c.calls[resp.ID]; ok {
			ch <- resp
			delete(c.calls, resp.ID)
		}
		c.mu.Unlock()
	}
}

var ErrWorkerDisconnected = errors.New("worker connection lost; the operation outcome is unknown until reconciled")

func (c *WorkerClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.next++
	id := fmt.Sprintf("w%d", c.next)
	ch := make(chan WorkerResponse, 1)
	c.calls[id] = ch
	c.mu.Unlock()
	raw, _ := json.Marshal(params)
	b, _ := json.Marshal(WorkerRequest{ID: id, Method: method, Params: raw})
	if _, err := c.conn.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWorkerDisconnected, err)
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, ErrWorkerDisconnected
		}
		if !resp.Success {
			return nil, errors.New(resp.Error)
		}
		return resp.Data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// RemoteTool executes a named tool on a worker. It declares ReplayReconcile:
// after an interruption the host asks the worker whether the operation
// completed before deciding anything. Connection loss during a call is an
// unknown outcome, never a failure that invites a retry.
type RemoteTool struct {
	Worker      *WorkerClient
	ToolName    string
	Desc        string
	ArgsSchema  json.RawMessage
	Epoch       func() uint64
	Environment string
}

func (t *RemoteTool) Name() string { return t.ToolName }
func (t *RemoteTool) Description() string {
	if t.Desc != "" {
		return t.Desc
	}
	return t.ToolName + " (remote)"
}
func (t *RemoteTool) Schema() json.RawMessage {
	if len(t.ArgsSchema) > 0 {
		return t.ArgsSchema
	}
	return json.RawMessage(`{"type":"object"}`)
}
func (t *RemoteTool) ReplayPolicy() core.ToolReplayPolicy { return core.ReplayReconcile }

func (t *RemoteTool) Execute(ctx context.Context, args json.RawMessage, progress func(string)) (core.ToolResult, error) {
	id, ok := ToolCallFromContext(ctx)
	if !ok {
		return core.ToolResult{IsError: true, Content: []provider.Content{provider.TextBlock{Text: "remote tools require a continuous run"}}}, nil
	}
	var epoch uint64
	if t.Epoch != nil {
		epoch = t.Epoch()
	}
	call := WorkerCall{OperationKey: id.OperationKey(), RunID: id.RunID, ConversationID: id.ConversationID, CallID: id.CallID, Epoch: epoch, Environment: t.Environment, Tool: t.ToolName, Args: args}
	data, err := t.Worker.call(ctx, "tool.execute", call)
	if err != nil {
		if errors.Is(err, ErrWorkerDisconnected) || ctx.Err() != nil {
			// Unknown outcome. Returning an error leaves the intent running
			// so recovery reconciles instead of inventing a result.
			return core.ToolResult{}, fmt.Errorf("%w: %w", core.ErrToolOutcomeUnknown, ErrWorkerDisconnected)
		}
		return core.ToolResult{IsError: true, Status: "failed", Content: []provider.Content{provider.TextBlock{Text: err.Error()}}}, nil
	}
	var result WorkerResult
	if err := json.Unmarshal(data, &result); err != nil {
		return core.ToolResult{}, err
	}
	return core.ToolResult{Content: decodeBlocks(result.Blocks), IsError: result.IsError, Status: result.Status}, nil
}

// Reconcile asks the worker about the operation. Workers without lookup, or
// unreachable workers, yield unknown.
func (t *RemoteTool) Reconcile(ctx context.Context, key string, args json.RawMessage) (core.ReconcileOutcome, core.ToolResult, error) {
	if !t.Worker.Hello.Lookup {
		return core.ReconcileUnknown, core.ToolResult{}, nil
	}
	data, err := t.Worker.call(ctx, "tool.lookup", WorkerLookup{OperationKey: key})
	if err != nil {
		return core.ReconcileUnknown, core.ToolResult{}, err
	}
	var res WorkerLookupResult
	if err := json.Unmarshal(data, &res); err != nil {
		return core.ReconcileUnknown, core.ToolResult{}, err
	}
	switch res.State {
	case "completed":
		if res.Result == nil {
			return core.ReconcileUnknown, core.ToolResult{}, nil
		}
		return core.ReconcileCompleted, core.ToolResult{Content: decodeBlocks(res.Result.Blocks), IsError: res.Result.IsError, Status: res.Result.Status}, nil
	case "not_started":
		return core.ReconcileNotStarted, core.ToolResult{}, nil
	}
	return core.ReconcileUnknown, core.ToolResult{}, nil
}
