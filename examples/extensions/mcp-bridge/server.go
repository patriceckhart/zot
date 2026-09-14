// server.go — MCP server process lifecycle management.
//
// Each configured MCP server is wrapped in a managedServer that tracks:
//   - Connection state (stopped / starting / ready)
//   - Last-access time (for idle timeout)
//   - Discovered tools
//
// Smart lazy: servers are spawned on startup to discover tools, then
// killed after an idle period. On the next tool call, they're respawned.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// serverState tracks the lifecycle of one MCP server.
type serverState int

const (
	stateStopped  serverState = iota // not running
	stateStarting                    // spawning / initializing
	stateReady                       // connected, tools discovered
	stateError                       // failed to start
)

func (s serverState) String() string {
	switch s {
	case stateStopped:
		return "stopped"
	case stateStarting:
		return "starting"
	case stateReady:
		return "ready"
	case stateError:
		return "error"
	default:
		return "unknown"
	}
}

// managedServer wraps one MCP server with lifecycle management.
type managedServer struct {
	name   string
	config ServerConfig
	cwd    string
	logger *log.Logger

	mu       sync.Mutex
	state    serverState
	client   *client.Client
	tools    []mcp.Tool
	lastUsed time.Time
	startErr error
	gen      uint64 // bumped by stop(); a start attempt only commits if unchanged
}

// newManagedServer creates a new server wrapper.
func newManagedServer(name string, cfg ServerConfig, cwd string, logger *log.Logger) *managedServer {
	return &managedServer{
		name:     name,
		config:   cfg,
		cwd:      cwd,
		logger:   logger,
		state:    stateStopped,
		lastUsed: time.Now(),
	}
}

// start spawns the MCP server process and discovers its tools.
// Safe to call concurrently; a stop() issued while a start is in
// flight wins — the late result is discarded and its client closed.
func (s *managedServer) start(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.config.ConnectTimeout)*time.Second)
	defer cancel()

	s.mu.Lock()
	if s.state == stateReady {
		s.lastUsed = time.Now()
		s.mu.Unlock()
		return nil
	}
	if s.state == stateStarting {
		s.mu.Unlock()
		return s.waitForReady(ctx)
	}
	s.state = stateStarting
	s.startErr = nil
	gen := s.gen
	s.mu.Unlock()

	c, tools, err := s.doStart(ctx)
	return s.finishStart(gen, c, tools, err)
}

// finishStart commits the outcome of a start attempt, unless stop()
// bumped the generation in the meantime.
func (s *managedServer) finishStart(gen uint64, c *client.Client, tools []mcp.Tool, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.gen != gen {
		if c != nil {
			c.Close()
		}
		return fmt.Errorf("[%s] stopped while starting", s.name)
	}
	if err != nil {
		s.state = stateError
		s.startErr = err
		s.logger.Printf("[%s] start failed: %v", s.name, err)
		return err
	}
	s.client = c
	s.tools = tools
	s.state = stateReady
	s.lastUsed = time.Now()
	s.logger.Printf("[%s] ready with %d tools", s.name, len(s.tools))
	return nil
}

// doStart performs the actual spawn + initialize + tools/list.
func (s *managedServer) doStart(ctx context.Context) (*client.Client, []mcp.Tool, error) {
	var c *client.Client
	var err error

	switch s.config.Transport {
	case "stdio", "":
		c, err = s.startStdio()
	case "streamable-http":
		c, err = s.startStreamableHTTP(ctx)
	case "sse":
		c, err = s.startSSE(ctx)
	default:
		return nil, nil, fmt.Errorf("unknown transport: %s", s.config.Transport)
	}

	if err != nil {
		return nil, nil, err
	}

	// Initialize the MCP session
	_, err = c.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo: mcp.Implementation{
				Name:    "zot-mcp-bridge",
				Version: "1.0.0",
			},
			Capabilities: mcp.ClientCapabilities{},
		},
	})
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("initialize: %w", err)
	}

	// Discover tools
	result, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		c.Close()
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}

	return c, result.Tools, nil
}

// startStdio spawns a stdio-based MCP server.
func (s *managedServer) startStdio() (*client.Client, error) {
	if s.config.Command == "" {
		return nil, fmt.Errorf("stdio transport requires 'command' field")
	}

	// mcp-go merges this slice with os.Environ() before spawning the subprocess.
	env := make([]string, 0, len(s.config.Env))
	for k, v := range s.config.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	// Create stdio client (this spawns the subprocess). Run stdio MCP
	// servers from the zot session's project directory, not the installed
	// extension directory. Many MCP servers use their process cwd as their
	// project root when answering relative-path or directory-listing
	// requests, so inheriting mcp-bridge's cwd makes them operate on the
	// extension install instead of the user's project.
	c, err := client.NewStdioMCPClientWithOptions(
		s.config.Command,
		env,
		s.config.Args,
		transport.WithCommandFunc(func(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error) {
			cmd := exec.CommandContext(ctx, command, args...)
			cmd.Env = append(os.Environ(), env...)
			if s.cwd != "" {
				cmd.Dir = s.cwd
			}
			return cmd, nil
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", s.config.Command, err)
	}

	// Capture stderr for debugging
	if stderr, ok := client.GetStderr(c); ok {
		go s.pipeStderr(stderr)
	}

	return c, nil
}

// startStreamableHTTP connects to an HTTP-based MCP server.
func (s *managedServer) startStreamableHTTP(ctx context.Context) (*client.Client, error) {
	if s.config.URL == "" {
		return nil, fmt.Errorf("streamable-http transport requires 'url' field")
	}

	// Build HTTP options
	opts := []transport.StreamableHTTPCOption{}

	// Streamable HTTP requires Accept: application/json, text/event-stream.
	// This is a protocol detail, so set it automatically. User-provided
	// headers are merged on top for auth/customization.
	headers := map[string]string{
		"Accept": "application/json, text/event-stream",
	}
	for k, v := range s.config.Headers {
		headers[k] = v
	}
	opts = append(opts, transport.WithHTTPHeaders(headers))
	oauth, err := s.oauthConfig()
	if err != nil { return nil, err }
	if oauth != nil { opts = append(opts, transport.WithHTTPOAuth(*oauth)) }

	// Create streamable HTTP client
	c, err := client.NewStreamableHttpClient(s.config.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("create HTTP client: %w", err)
	}

	// Start the transport
	if err := c.GetTransport().Start(ctx); err != nil {
		return nil, fmt.Errorf("start HTTP transport: %w", err)
	}

	s.logger.Printf("[%s] connected to %s", s.name, s.config.URL)
	return c, nil
}

// startSSE connects to an SSE-based MCP server (legacy transport).
func (s *managedServer) startSSE(ctx context.Context) (*client.Client, error) {
	if s.config.URL == "" {
		return nil, fmt.Errorf("sse transport requires 'url' field")
	}

	// Create SSE client
	var opts []transport.ClientOption
	oauth, err := s.oauthConfig()
	if err != nil { return nil, err }
	if oauth != nil { opts = append(opts, transport.WithOAuth(*oauth)) }
	c, err := client.NewSSEMCPClient(s.config.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("create SSE client: %w", err)
	}

	// Start the transport
	if err := c.GetTransport().Start(ctx); err != nil {
		return nil, fmt.Errorf("start SSE transport: %w", err)
	}

	s.logger.Printf("[%s] connected to SSE server at %s", s.name, s.config.URL)
	return c, nil
}

// pipeStderr reads server stderr line by line and logs it.
func (s *managedServer) pipeStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" {
			s.logger.Printf("[%s:stderr] %s", s.name, line)
		}
	}
}

// stop gracefully shuts down the server.
func (s *managedServer) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.gen++ // invalidate any start attempt still in flight
	if s.client != nil {
		s.client.Close()
		s.client = nil
	}
	if s.state != stateError {
		s.state = stateStopped
	}
}

// callTool forwards a tool call to the MCP server.
// If the server is not running, it starts it first.
func (s *managedServer) callTool(ctx context.Context, toolName string, args json.RawMessage) (*mcp.CallToolResult, error) {
	s.mu.Lock()
	c := s.client
	st := s.state
	s.mu.Unlock()

	// If not ready, start the server
	if st != stateReady || c == nil {
		if err := s.start(ctx); err != nil {
			return nil, err
		}
		s.mu.Lock()
		c = s.client
		s.mu.Unlock()
		if c == nil {
			return nil, fmt.Errorf("[%s] stopped before the call could run", s.name)
		}
	}

	// Parse args into map[string]any
	var argsMap map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argsMap); err != nil {
			return nil, fmt.Errorf("invalid args: %w", err)
		}
	}

	// Call the tool
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(s.config.RequestTimeout)*time.Second)
	defer cancel()

	result, err := c.CallTool(callCtx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      toolName,
			Arguments: argsMap,
		},
	})
	if err != nil {
		return nil, err
	}

	// Update last-used time
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()

	return result, nil
}

// waitForReady blocks until the server is ready or the context
// expires. Polling is a deliberate simplification for this example;
// a channel closed by finishStart would avoid the 100ms latency.
func (s *managedServer) waitForReady(ctx context.Context) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			s.mu.Lock()
			st := s.state
			err := s.startErr
			s.mu.Unlock()
			if st == stateReady {
				return nil
			}
			if st == stateError {
				return err
			}
		}
	}
}

// isIdle returns true if the server hasn't been used within the timeout.
func (s *managedServer) isIdle(timeout time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != stateReady {
		return false
	}
	return time.Since(s.lastUsed) > timeout
}

// status returns a compact human-readable status string.
func (s *managedServer) status() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch s.state {
	case stateReady:
		return compactToolStatus(s.name, len(s.tools))
	case stateError:
		if s.startErr != nil {
			return fmt.Sprintf("%s (%s)", s.name, compactErr(s.startErr.Error()))
		}
		return fmt.Sprintf("%s (error)", s.name)
	case stateStopped:
		if len(s.tools) > 0 {
			return fmt.Sprintf("%s (sleeping, %s)", s.name, toolCountText(len(s.tools)))
		}
		return fmt.Sprintf("%s (sleeping)", s.name)
	case stateStarting:
		return fmt.Sprintf("%s (starting)", s.name)
	default:
		return fmt.Sprintf("%s (%s)", s.name, s.state)
	}
}

// detailStatus returns a multi-line status for one server.
func (s *managedServer) detailStatus() string {
	s.mu.Lock()
	name := s.name
	transport := s.config.Transport
	url := s.config.URL
	command := s.config.Command
	args := append([]string(nil), s.config.Args...)
	state := s.state
	toolCount := len(s.tools)
	idleTimeout := s.config.IdleTimeout
	requestTimeout := s.config.RequestTimeout
	connectTimeout := s.config.ConnectTimeout
	lastUsed := s.lastUsed
	startErr := s.startErr
	s.mu.Unlock()

	if transport == "" {
		transport = "stdio"
	}

	var sb strings.Builder
	sb.WriteString("MCP server: ")
	sb.WriteString(name)
	sb.WriteByte('\n')
	sb.WriteString("  status: ")
	sb.WriteString(state.String())
	if state == stateError && startErr != nil {
		sb.WriteString(" (")
		sb.WriteString(compactErr(startErr.Error()))
		sb.WriteByte(')')
	}
	sb.WriteByte('\n')
	sb.WriteString("  tools: ")
	sb.WriteString(toolCountText(toolCount))
	sb.WriteByte('\n')
	sb.WriteString("  transport: ")
	sb.WriteString(transport)
	sb.WriteByte('\n')
	if url != "" {
		sb.WriteString("  url: ")
		sb.WriteString(url)
		sb.WriteByte('\n')
	}
	if command != "" {
		sb.WriteString("  command: ")
		sb.WriteString(command)
		if len(args) > 0 {
			sb.WriteByte(' ')
			sb.WriteString(strings.Join(args, " "))
		}
		sb.WriteByte('\n')
	}
	sb.WriteString(fmt.Sprintf("  timeouts: connect=%ds request=%ds idle=%ds\n", connectTimeout, requestTimeout, idleTimeout))
	if !lastUsed.IsZero() {
		sb.WriteString("  last used: ")
		sb.WriteString(time.Since(lastUsed).Round(time.Second).String())
		sb.WriteString(" ago\n")
	}
	if state == stateError && startErr != nil {
		sb.WriteString("  error: ")
		sb.WriteString(firstLine(startErr.Error()))
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

func compactToolStatus(name string, n int) string {
	if n <= 0 {
		return name
	}
	return fmt.Sprintf("%s (%s)", name, toolCountText(n))
}

func toolCountText(n int) string {
	if n == 1 {
		return "1 tool"
	}
	return fmt.Sprintf("%d tools", n)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\n\r"); i >= 0 {
		s = s[:i]
	}
	return s
}

func compactErr(s string) string {
	s = firstLine(s)
	if s == "" {
		return "error"
	}
	lower := strings.ToLower(s)
	switch {
	case strings.Contains(lower, "authorization") || strings.Contains(lower, "unauthorized") || strings.Contains(lower, "401") || strings.Contains(lower, "403"):
		return "auth failed"
	case strings.Contains(lower, "transport") || strings.Contains(lower, "connection") || strings.Contains(lower, "connect"):
		return "connection failed"
	case strings.Contains(lower, "context deadline exceeded") || strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out"):
		return "timeout"
	case strings.Contains(lower, "no such file") || strings.Contains(lower, "executable file not found"):
		return "command not found"
	case strings.HasPrefix(lower, "initialize:"):
		return "initialize failed"
	case strings.HasPrefix(lower, "list tools:"):
		return "tool discovery failed"
	}
	const max = 44
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}
