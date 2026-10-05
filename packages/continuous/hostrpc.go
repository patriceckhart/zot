package continuous

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/core"
)

// The host protocol is newline-delimited JSON, one object per line in each
// direction, like zot rpc. A client sends requests with an id and a method;
// the host answers with a response carrying the same id, and pushes events
// without an id for watches. Every method is authorized through the
// connection's role; the first line must be hello with the shared token when
// the host was started with one.
//
// Methods (request params and response data in JSON):
//
//	hello            {token}                           -> {role, version}
//	runtime.status   {}                                -> Status + blocked run IDs
//	conversation.create  {workspace, config}           -> Conversation
//	conversation.list    {after, limit}                -> {conversations, revision, after}
//	conversation.snapshot {id, limit}                  -> ConversationSnapshot
//	conversation.watch   {id, after}                   -> streams event {type:"commit", id, commit} until cancelled
//	conversation.submit  {id, content, request_id, when_busy} -> Submission
//	conversation.configure {id, expected_revision, config}   -> Conversation
//	conversation.abort   {id}                          -> Run
//	conversation.compact {id, instructions, keep}      -> Conversation
//	conversation.reset   {id, handoff}                 -> Conversation
//	conversation.fork    {id, at, config}              -> Conversation
//	submission.get       {id}                          -> Submission
//	submission.wait      {id}                          -> Submission (blocks)
//	usage.get            {id}                          -> UsageTotals
//	recovery.preview     {}                            -> RecoveryPlan
//	recovery.unblock     {run_id}                      -> {}
//	watch.cancel         {watch_id}                    -> {}
//
// Roles: read (snapshot, watch, list, get, status), submit (+submit,
// configure, compact, reset, fork), admin (+abort, unblock, create). A token
// grants the role it was configured with. Without a token the connection is
// admin, which is only acceptable on a private local socket.

// Role is a client capability level.
type Role string

const (
	RoleRead Role = "read"
	// RoleSubmit may create, submit, steer, configure, compact, reset, fork,
	// and write documents.
	RoleSubmit Role = "submit"
	// RoleApprove is submit plus approval decisions.
	RoleApprove Role = "approve"
	// RoleAdmin is everything, including abort and recovery unblock.
	RoleAdmin Role = "admin"
)

// ValidRole reports whether r is a known role.
func ValidRole(r Role) bool {
	return r == RoleRead || r == RoleSubmit || r == RoleApprove || r == RoleAdmin
}

func (r Role) allows(method string) bool {
	switch method {
	case "hello", "runtime.status", "conversation.list", "conversation.snapshot", "conversation.watch", "submission.get", "submission.wait", "usage.get", "recovery.preview", "watch.cancel", "approval.get", "approval.list", "task.get", "task.list", "document.read", "budget.get", "conversation.search", "memo.get", "prompt.records", "prompt.section", "partial.get", "outbox.list":
		return true
	case "conversation.create", "conversation.submit", "conversation.configure", "conversation.compact", "conversation.reset", "conversation.fork", "document.write", "memo.set":
		return r == RoleSubmit || r == RoleApprove || r == RoleAdmin
	case "approval.decide":
		return r == RoleApprove || r == RoleAdmin
	case "conversation.abort", "recovery.unblock", "task.abort", "task.retry-cleanup", "budget.set", "runtime.reload", "runtime.retain", "outbox.ack", "submission.withdraw", "submission.reorder":
		return r == RoleAdmin
	}
	return false
}

// HostServer serves the host protocol to clients. Token maps shared secrets to
// roles. An empty Tokens map grants admin to every connection; use it only on
// a private local socket.
type HostServer struct {
	Host    *Host
	Engine  Engine
	Tokens  map[string]Role
	Version string
	// MaxWatches bounds concurrent watches per connection. Zero means 16.
	MaxWatches int
	// MaxQueue bounds unclaimed submissions per conversation admitted through
	// this server. Zero means 64. Excess returns code queue_full.
	MaxQueue int
	// MaxConnections bounds concurrent client connections. Zero means 256.
	MaxConnections int
	// Documents serves document.read and document.write. Nil reports
	// unsupported.
	Documents DocumentRegistry
	// Reload builds a replacement engine for runtime.reload (for example by
	// rediscovering extensions). Nil reports unsupported. The host validates
	// and publishes the engine atomically; running calls keep the old one.
	Reload func(ctx context.Context) (Engine, error)
	// SearchIndex accelerates conversation.search text queries when set.
	// Results are identical to the sequential scan.
	SearchIndex *SearchIndex
}

var ErrUnsupported = errors.New("continuous: unsupported capability")

type hostRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type hostResponse struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Method  string `json:"method,omitempty"`
	Success bool   `json:"success"`
	Data    any    `json:"data,omitempty"`
	Error   string `json:"error,omitempty"`
	// Code distinguishes error classes for clients.
	Code string `json:"code,omitempty"`
}

// Serve accepts connections until ctx ends or the listener fails. Each
// connection runs its own goroutine. Closing the listener stops Serve.
func (s *HostServer) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	limit := s.MaxConnections
	if limit <= 0 {
		limit = 256
	}
	slots := make(chan struct{}, limit)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		select {
		case slots <- struct{}{}:
		default:
			// Over the limit: refuse with a protocol error instead of
			// holding a goroutine and buffers for the connection.
			b, _ := json.Marshal(hostResponse{Type: "response", Success: false, Error: "connection limit reached", Code: "limit"})
			conn.Write(append(b, '\n'))
			conn.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.ServeConn(ctx, conn)
		}()
	}
}

// ServeConn serves one connection until it closes or ctx ends.
func (s *HostServer) ServeConn(ctx context.Context, conn io.ReadWriteCloser) {
	c := &hostConn{server: s, conn: conn, watches: map[string]context.CancelFunc{}}
	defer c.close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	c.role = RoleAdmin
	needHello := len(s.Tokens) > 0
	if needHello {
		c.role = ""
	}
	reader := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		line = []byte(strings.TrimSpace(string(line)))
		if len(line) == 0 {
			continue
		}
		var req hostRequest
		if err := json.Unmarshal(line, &req); err != nil || req.Method == "" {
			c.write(hostResponse{ID: req.ID, Type: "response", Success: false, Error: "malformed request", Code: "bad_request"})
			continue
		}
		if c.role == "" && req.Method != "hello" {
			c.write(hostResponse{ID: req.ID, Type: "response", Method: req.Method, Success: false, Error: "hello required", Code: "unauthorized"})
			return
		}
		if req.Method == "hello" {
			var p struct {
				Token string `json:"token"`
			}
			_ = json.Unmarshal(req.Params, &p)
			role, ok := s.authenticate(p.Token)
			if !ok {
				c.write(hostResponse{ID: req.ID, Type: "response", Method: req.Method, Success: false, Error: "invalid token", Code: "unauthorized"})
				return
			}
			c.role = role
			c.write(hostResponse{ID: req.ID, Type: "response", Method: req.Method, Success: true, Data: map[string]any{"role": role, "version": s.Version}})
			continue
		}
		if !c.role.allows(req.Method) {
			c.write(hostResponse{ID: req.ID, Type: "response", Method: req.Method, Success: false, Error: "method not allowed for role " + string(c.role), Code: "forbidden"})
			continue
		}
		// Blocking methods run concurrently so a watch or wait never stalls
		// the connection. Short methods run inline to preserve ordering.
		switch req.Method {
		case "conversation.watch", "submission.wait":
			go c.handle(ctx, req)
		default:
			c.handle(ctx, req)
		}
	}
}

func (s *HostServer) authenticate(token string) (Role, bool) {
	if len(s.Tokens) == 0 {
		return RoleAdmin, true
	}
	for candidate, role := range s.Tokens {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			return role, true
		}
	}
	return "", false
}

type hostConn struct {
	server  *HostServer
	conn    io.ReadWriteCloser
	role    Role
	writeMu sync.Mutex
	mu      sync.Mutex
	watches map[string]context.CancelFunc
}

func (c *hostConn) close() {
	c.mu.Lock()
	for _, cancel := range c.watches {
		cancel()
	}
	c.watches = nil
	c.mu.Unlock()
	c.conn.Close()
}

func (c *hostConn) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, _ = c.conn.Write(append(b, '\n'))
}

func errorCode(err error) string {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrTaskNotFound), errors.Is(err, ErrNoRun), errors.Is(err, ErrApprovalNotFound):
		return "not_found"
	case errors.Is(err, storage.ErrConflict), errors.Is(err, ErrDocumentConflict), errors.Is(err, ErrApprovalDecided):
		return "conflict"
	case errors.Is(err, ErrRequestConflict), errors.Is(err, ErrSessionConflict):
		return "duplicate_key"
	case errors.Is(err, ErrBusy):
		return "busy"
	case errors.Is(err, ErrQueueFull):
		return "queue_full"
	case errors.Is(err, ErrBudgetExceeded):
		return "budget_exceeded"
	case errors.Is(err, ErrNotQueued):
		return "conflict"
	case errors.Is(err, storage.ErrCursor):
		return "cursor_expired"
	case errors.Is(err, storage.ErrCorrupt):
		return "storage"
	case errors.Is(err, ErrTaskBlocked), errors.Is(err, ErrDocumentBlocked), errors.Is(err, ErrAwaitingApproval):
		return "blocked"
	case errors.Is(err, ErrUnsupported):
		return "unsupported"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	}
	return "error"
}

func (c *hostConn) handle(ctx context.Context, req hostRequest) {
	data, err := c.dispatch(ctx, req)
	if errors.Is(err, errWatchHandled) {
		return
	}
	if err != nil {
		c.write(hostResponse{ID: req.ID, Type: "response", Method: req.Method, Success: false, Error: err.Error(), Code: errorCode(err)})
		return
	}
	c.write(hostResponse{ID: req.ID, Type: "response", Method: req.Method, Success: true, Data: data})
}

func (c *hostConn) dispatch(ctx context.Context, req hostRequest) (any, error) {
	r := c.server.Host.Runtime()
	params := func(v any) error {
		if len(req.Params) == 0 {
			return nil
		}
		return json.Unmarshal(req.Params, v)
	}
	switch req.Method {
	case "runtime.status":
		status, err := r.Status(ctx)
		if err != nil {
			return nil, err
		}
		metrics, err := c.server.Host.Metrics(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"status": status, "blocked_runs": c.server.Host.Blocked(), "metrics": metrics, "version": c.server.Version}, nil
	case "conversation.create":
		var p struct {
			Workspace string       `json:"workspace"`
			Config    *AgentConfig `json:"config"`
			// Owner and Key create an owned conversation instead of a
			// workspace root: the child is created once per (owner ID, key)
			// under Owner.ConversationID and inherits that conversation's
			// configuration unless Config is given.
			Owner *Owner `json:"owner,omitempty"`
			Key   string `json:"key,omitempty"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		if p.Owner != nil {
			return r.CreateOwnedConversation(ctx, p.Owner.ConversationID, p.Owner.ID, p.Key, p.Config)
		}
		var config AgentConfig
		if p.Config != nil {
			config = *p.Config
		}
		return r.OpenRoot(ctx, p.Workspace, config)
	case "conversation.list":
		var p struct {
			After string `json:"after"`
			Limit int    `json:"limit"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		if p.Limit <= 0 {
			p.Limit = 100
		}
		list, revision, err := r.Conversations(ctx, p.After, p.Limit)
		if err != nil {
			return nil, err
		}
		after := ""
		if len(list) > 0 {
			after = list[len(list)-1].ID
		}
		return map[string]any{"conversations": list, "revision": revision, "after": after}, nil
	case "conversation.snapshot":
		var p struct {
			ID    string `json:"id"`
			Limit int    `json:"limit"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		if p.Limit <= 0 {
			p.Limit = 200
		}
		return r.ConversationSnapshot(ctx, p.ID, p.Limit)
	case "conversation.watch":
		var p struct {
			ID    string `json:"id"`
			After uint64 `json:"after"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return c.watch(ctx, req.ID, p.ID, p.After)
	case "watch.cancel":
		var p struct {
			WatchID string `json:"watch_id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		c.mu.Lock()
		cancel, ok := c.watches[p.WatchID]
		delete(c.watches, p.WatchID)
		c.mu.Unlock()
		if ok {
			cancel()
		}
		return map[string]any{}, nil
	case "conversation.submit":
		var p struct {
			ID        string `json:"id"`
			Content   string `json:"content"`
			RequestID string `json:"request_id"`
			WhenBusy  string `json:"when_busy"`
			Policy    string `json:"policy"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		maxQueue := c.server.MaxQueue
		if maxQueue <= 0 {
			maxQueue = 64
		}
		opts := SubmitOptions{Policy: p.Policy, RejectBusy: p.WhenBusy == "reject", MaxQueue: maxQueue}
		if p.WhenBusy == "steer" {
			opts.Policy = PolicySteer
		}
		s, err := r.SubmitWith(ctx, p.ID, "client:"+string(c.role), p.RequestID, p.Content, opts)
		if err != nil {
			return nil, err
		}
		c.server.Host.Nudge()
		return s, nil
	case "conversation.configure":
		var p struct {
			ID               string      `json:"id"`
			ExpectedRevision uint64      `json:"expected_revision"`
			Config           AgentConfig `json:"config"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.Configure(ctx, p.ID, p.ExpectedRevision, p.Config)
	case "conversation.abort":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		run, err := r.Abort(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		c.server.Host.Nudge()
		return run, nil
	case "conversation.compact":
		var p struct {
			ID           string `json:"id"`
			Instructions string `json:"instructions"`
			Keep         int    `json:"keep"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.Compact(ctx, c.server.Engine, p.ID, p.Instructions, p.Keep)
	case "conversation.reset":
		var p struct {
			ID      string `json:"id"`
			Handoff string `json:"handoff"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.Reset(ctx, p.ID, p.Handoff)
	case "conversation.fork":
		var p struct {
			ID     string       `json:"id"`
			At     uint64       `json:"at"`
			Config *AgentConfig `json:"config"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.Fork(ctx, p.ID, p.At, p.Config)
	case "submission.get":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.Submission(ctx, p.ID)
	case "submission.wait":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.WaitSubmission(ctx, p.ID)
	case "usage.get":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		if _, err := r.Conversation(ctx, p.ID); err != nil {
			return nil, err
		}
		return r.Usage(ctx, p.ID)
	case "recovery.preview":
		return r.RecoveryPreview(ctx)
	case "recovery.unblock":
		var p struct {
			RunID string `json:"run_id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		c.server.Host.Unblock(p.RunID)
		return map[string]any{}, nil
	case "approval.get":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.Approval(ctx, p.ID)
	case "approval.list":
		var p struct {
			Conversation string `json:"conversation"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		list, err := r.PendingApprovals(ctx, p.Conversation)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []Approval{}
		}
		return map[string]any{"approvals": list}, nil
	case "approval.decide":
		var p struct {
			ID     string `json:"id"`
			Allow  bool   `json:"allow"`
			Scope  string `json:"scope"`
			Reason string `json:"reason"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		a, err := r.Decide(ctx, p.ID, "client:"+string(c.role), p.Allow, p.Scope, p.Reason)
		if err != nil {
			return nil, err
		}
		c.server.Host.Nudge()
		return a, nil
	case "task.get":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.Task(ctx, p.ID)
	case "task.list":
		var p struct {
			Conversation string `json:"conversation"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		list, err := r.Tasks(ctx, p.Conversation)
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []Task{}
		}
		return map[string]any{"tasks": list}, nil
	case "task.abort":
		var p struct {
			ID                string `json:"id"`
			IncludeBackground bool   `json:"include_background"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		if err := r.AbortTask(ctx, p.ID, p.IncludeBackground); err != nil {
			return nil, err
		}
		c.server.Host.Nudge()
		return r.Task(ctx, p.ID)
	case "conversation.search":
		var q SearchQuery
		if err := params(&q); err != nil {
			return nil, err
		}
		return r.SearchIndexed(ctx, c.server.SearchIndex, q)
	case "outbox.list":
		list, err := r.Outbox(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"notifications": list}, nil
	case "outbox.ack":
		// An external integration that delivered a notification itself
		// acknowledges it here; the host's own Notifier acknowledges on
		// success automatically.
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		ok, err := r.Acknowledge(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"acknowledged": ok}, nil
	case "submission.withdraw":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.Withdraw(ctx, p.ID, "client:"+string(c.role))
	case "submission.reorder":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		queue, err := r.Reorder(ctx, p.ID, "client:"+string(c.role))
		if err != nil {
			return nil, err
		}
		c.server.Host.Nudge()
		return map[string]any{"queue": queue}, nil
	case "runtime.retain":
		var p struct {
			Policy RetentionPolicy `json:"policy"`
			DryRun bool            `json:"dry_run"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.Retain(ctx, p.Policy, p.DryRun)
	case "memo.get":
		var p struct {
			Scope string `json:"scope"`
			Key   string `json:"key"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		m, ok, err := r.Memo(ctx, p.Scope, p.Key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: memo", ErrNotFound)
		}
		return m, nil
	case "memo.set":
		var p struct {
			Scope string          `json:"scope"`
			Key   string          `json:"key"`
			Value json.RawMessage `json:"value"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		m, created, err := r.Memoize(ctx, p.Scope, p.Key, "client:"+string(c.role), p.Value)
		if err != nil {
			return nil, err
		}
		return map[string]any{"memo": m, "created": created}, nil
	case "prompt.records":
		var p struct {
			Conversation string `json:"conversation"`
			Limit        int    `json:"limit"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		list, err := r.PromptRecords(ctx, p.Conversation, p.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"records": list}, nil
	case "prompt.section":
		var p struct {
			Hash string `json:"hash"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		sec, ok, err := r.PromptSection(ctx, p.Hash)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: prompt section", ErrNotFound)
		}
		return sec, nil
	case "partial.get":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		partial, ok, err := r.Partial(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return map[string]any{}, nil
		}
		return partial, nil
	case "runtime.reload":
		if c.server.Reload == nil {
			return nil, fmt.Errorf("%w: host has no reload hook", ErrUnsupported)
		}
		engine, err := c.server.Reload(ctx)
		if err != nil {
			return nil, err
		}
		gen, err := c.server.Host.Reload(ctx, engine)
		if err != nil {
			return nil, err
		}
		return map[string]any{"generation": gen}, nil
	case "task.retry-cleanup":
		var p struct {
			ID string `json:"id"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		t, err := r.RetryCleanup(ctx, p.ID, "client:"+string(c.role))
		if err != nil {
			return nil, err
		}
		c.server.Host.Nudge()
		return t, nil
	case "budget.get":
		var p struct {
			Scope        string `json:"scope"`
			Conversation string `json:"conversation"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		if p.Scope == "" {
			p.Scope = "conversation"
		}
		statuses, err := r.CheckBudgets(ctx, p.Conversation)
		if err != nil && !errors.Is(err, ErrBudgetExceeded) {
			return nil, err
		}
		if statuses == nil {
			statuses = []BudgetStatus{}
		}
		return map[string]any{"budgets": statuses, "exceeded": err != nil}, nil
	case "budget.set":
		var p struct {
			Budget           Budget `json:"budget"`
			ExpectedRevision uint64 `json:"expected_revision"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		return r.SetBudget(ctx, p.Budget, p.ExpectedRevision)
	case "document.read":
		var p struct {
			Kind         string `json:"kind"`
			Conversation string `json:"conversation"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		if c.server.Documents == nil {
			return nil, fmt.Errorf("%w: host has no document registry", ErrUnsupported)
		}
		return r.ReadDocument(ctx, c.server.Documents, p.Kind, p.Conversation)
	case "document.write":
		var p struct {
			Kind             string          `json:"kind"`
			Conversation     string          `json:"conversation"`
			ExpectedRevision uint64          `json:"expected_revision"`
			Value            json.RawMessage `json:"value"`
			Delete           bool            `json:"delete"`
		}
		if err := params(&p); err != nil {
			return nil, err
		}
		if c.server.Documents == nil {
			return nil, fmt.Errorf("%w: host has no document registry", ErrUnsupported)
		}
		if p.Delete {
			return r.DeleteDocument(ctx, c.server.Documents, p.Kind, p.Conversation, p.ExpectedRevision)
		}
		return r.WriteDocument(ctx, c.server.Documents, p.Kind, p.Conversation, p.ExpectedRevision, p.Value)
	}
	return nil, fmt.Errorf("%w: unknown method %q", ErrUnsupported, req.Method)
}

// watch streams commits for a conversation as events until cancelled. The
// response to the watch request carries the watch id; events follow with
// type commit and the same watch id. A client behind by more than the
// retained history receives a cursor_expired error and must resnapshot.
func (c *hostConn) watch(ctx context.Context, watchID, conversationID string, after uint64) (any, error) {
	if watchID == "" {
		return nil, fmt.Errorf("watch requires a request id")
	}
	max := c.server.MaxWatches
	if max <= 0 {
		max = 16
	}
	c.mu.Lock()
	if c.watches == nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("connection closed")
	}
	if len(c.watches) >= max {
		c.mu.Unlock()
		return nil, fmt.Errorf("watch limit %d reached", max)
	}
	if _, exists := c.watches[watchID]; exists {
		c.mu.Unlock()
		return nil, fmt.Errorf("watch id in use")
	}
	watchCtx, cancel := context.WithCancel(ctx)
	c.watches[watchID] = cancel
	c.mu.Unlock()
	c.write(hostResponse{ID: watchID, Type: "response", Method: "conversation.watch", Success: true, Data: map[string]any{"watch_id": watchID, "after": after}})
	err := c.server.Host.Runtime().Watch(watchCtx, conversationID, after, func(cm storage.Commit) error {
		// Deliver with a bounded write deadline so a stalled client cannot
		// hold host memory; the watch ends with an error instead.
		c.write(map[string]any{"type": "commit", "watch_id": watchID, "commit": cm})
		return nil
	})
	c.mu.Lock()
	if c.watches != nil {
		delete(c.watches, watchID)
	}
	c.mu.Unlock()
	if err != nil && !errors.Is(err, context.Canceled) {
		c.write(hostResponse{ID: watchID, Type: "response", Method: "conversation.watch", Success: false, Error: err.Error(), Code: errorCode(err)})
	} else {
		c.write(map[string]any{"type": "watch_end", "watch_id": watchID})
	}
	// The initial response was already written; return a sentinel so the
	// caller does not write a second one.
	return nil, errWatchHandled
}

var errWatchHandled = errors.New("watch handled")

// EventRow projects an engine event into the JSON the host streams to
// attached clients. It is shared with the CLI --json output.
func EventRow(ev core.AgentEvent) (map[string]any, bool) {
	switch e := ev.(type) {
	case core.EvTextDelta:
		return map[string]any{"type": "text", "text": e.Delta}, true
	case core.EvToolCall:
		return map[string]any{"type": "tool_call", "id": e.ID, "name": e.Name, "args": json.RawMessage(e.Args)}, true
	case core.EvToolProgress:
		return map[string]any{"type": "tool_progress", "id": e.ID, "text": e.Text}, true
	case core.EvToolResult:
		return map[string]any{"type": "tool_result", "id": e.ID, "name": e.Name, "status": e.Status, "is_error": e.Result.IsError, "text": core.ToolResultText(e.Result)}, true
	case core.EvUsage:
		return map[string]any{"type": "usage", "usage": e.Usage}, true
	case core.EvTurnEnd:
		row := map[string]any{"type": "turn_end", "stop": string(e.Stop)}
		if e.Err != nil {
			row["error"] = e.Err.Error()
		}
		return row, true
	case core.EvCompact:
		row := map[string]any{"type": e.Type(), "conversation": e.ID, "status": e.Status, "tokens": e.TokenEstimate}
		if e.Err != nil {
			row["error"] = e.Err.Error()
		}
		return row, true
	case HostError:
		return map[string]any{"type": "host_error", "conversation": e.ConversationID, "error": e.Err}, true
	case EvGap:
		return map[string]any{"type": "gap", "dropped": e.Dropped}, true
	}
	return nil, false
}
