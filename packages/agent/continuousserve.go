package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/patriceckhart/zot/packages/continuous"
	"github.com/patriceckhart/zot/packages/continuous/storage/journal"
)

// continuousServeOptions are the flags of `zot continuous serve`.
type continuousServeOptions struct {
	store       string
	backend     string
	durability  journal.Options
	socket      string
	listen      string
	tokenFile   string
	tlsCert     string
	tlsKey      string
	tlsClientCA string
	policy      continuous.RecoveryPolicy
	maxTurns    int
	hostArgs    []string
}

func parseContinuousServeArgs(args []string) (continuousServeOptions, error) {
	opts := continuousServeOptions{policy: continuous.RecoverSafe}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "--store":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.store = v
		case "--durability":
			v, err := value()
			if err != nil {
				return opts, err
			}
			if v != "strict" && v != "process" {
				return opts, fmt.Errorf("durability must be strict or process")
			}
			opts.durability.Durability = continuousDurability(v)
		case "--backend":
			v, err := value()
			if err != nil {
				return opts, err
			}
			if !validContinuousBackend(v) {
				return opts, fmt.Errorf("backend must be journal or sqlite")
			}
			opts.backend = v
		case "--socket":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.socket = v
		case "--listen":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.listen = v
		case "--token-file":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.tokenFile = v
		case "--tls-cert":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.tlsCert = v
		case "--tls-key":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.tlsKey = v
		case "--tls-client-ca":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.tlsClientCA = v
		case "--recover":
			v, err := value()
			if err != nil {
				return opts, err
			}
			switch continuous.RecoveryPolicy(v) {
			case continuous.RecoverSafe, continuous.RecoverAll, continuous.RecoverNone:
				opts.policy = continuous.RecoveryPolicy(v)
			default:
				return opts, fmt.Errorf("--recover must be safe, all, or none")
			}
		case "--max-turns":
			v, err := value()
			if err != nil {
				return opts, err
			}
			if _, err := fmt.Sscanf(v, "%d", &opts.maxTurns); err != nil || opts.maxTurns < 1 {
				return opts, fmt.Errorf("--max-turns requires a positive integer")
			}
		case "-p", "--print", "--stream", "--rpc", "-c", "--continue", "-r", "--session", "--no-session", "--swarm-agent", "--list-models", "--stats", "--json":
			return opts, fmt.Errorf("%s does not apply to continuous serve", arg)
		default:
			if !strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("continuous serve takes no positional arguments")
			}
			opts.hostArgs = append(opts.hostArgs, arg)
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && hostFlagTakesValue(arg) {
				i++
				opts.hostArgs = append(opts.hostArgs, args[i])
			}
		}
	}
	if opts.store == "" {
		return opts, fmt.Errorf("continuous serve requires --store")
	}
	if opts.socket != "" && opts.listen != "" {
		return opts, fmt.Errorf("use --socket or --listen, not both")
	}
	if (opts.tlsCert == "") != (opts.tlsKey == "") {
		return opts, fmt.Errorf("--tls-cert and --tls-key must be given together")
	}
	if opts.tlsCert != "" && opts.listen == "" {
		return opts, fmt.Errorf("--tls-cert applies to --listen")
	}
	if opts.tlsClientCA != "" && opts.tlsCert == "" {
		return opts, fmt.Errorf("--tls-client-ca requires --tls-cert and --tls-key")
	}
	if opts.listen != "" {
		host, _, err := net.SplitHostPort(opts.listen)
		if err != nil {
			return opts, fmt.Errorf("--listen requires host:port: %w", err)
		}
		ip := net.ParseIP(host)
		loopback := host == "localhost" || (ip != nil && ip.IsLoopback())
		if !loopback && opts.tlsCert == "" {
			return opts, fmt.Errorf("--listen on a non-loopback address requires --tls-cert and --tls-key; remote access is encrypted and authenticated or not offered")
		}
		if opts.tokenFile == "" {
			return opts, fmt.Errorf("--listen requires --token-file")
		}
	}
	if opts.socket == "" && opts.listen == "" {
		// On Windows the store-relative path names a private named pipe
		// (see continuous.LocalEndpointName) instead of a socket file.
		opts.socket = filepath.Join(opts.store, "host.sock")
	}
	return opts, nil
}

// serverTLSConfig builds a TLS 1.3 configuration for remote listeners. When
// clientCA is set, clients must present a certificate signed by it (mutual
// TLS); tokens are still required on top for role assignment.
func serverTLSConfig(certFile, keyFile, clientCA string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	if clientCA != "" {
		pem, err := os.ReadFile(clientCA)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates found in %s", clientCA)
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// loadTokens reads a token file: one `token role` pair per line, roles read,
// submit, or admin. The file must not be world readable on Unix.
func loadTokens(path string) (map[string]continuous.Role, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("token file %s must not be group or world accessible (mode %o)", path, info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	tokens := map[string]continuous.Role{}
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("token file line %d: expected <token> <role>", n+1)
		}
		role := continuous.Role(fields[1])
		if !continuous.ValidRole(role) {
			return nil, fmt.Errorf("token file line %d: unknown role %q", n+1, fields[1])
		}
		if len(fields[0]) < 16 {
			return nil, fmt.Errorf("token file line %d: tokens must be at least 16 characters", n+1)
		}
		tokens[fields[0]] = role
	}
	if len(tokens) == 0 {
		return nil, fmt.Errorf("token file %s defines no tokens", path)
	}
	return tokens, nil
}

// runContinuousServe hosts a store: it recovers interrupted runs according
// to the policy, executes queued work, and serves the host protocol to local
// clients. It runs until interrupted. Work interrupted by shutdown stays
// recoverable.
func runContinuousServe(ctx context.Context, args []string, out io.Writer) (retErr error) {
	opts, err := parseContinuousServeArgs(args)
	if err != nil {
		return err
	}
	hostArgs, err := ParseArgs(append([]string{"--print"}, opts.hostArgs...))
	if err != nil {
		return err
	}
	r, err := Resolve(hostArgs, true)
	if err != nil {
		return err
	}
	var tokens map[string]continuous.Role
	if opts.tokenFile != "" {
		if tokens, err = loadTokens(opts.tokenFile); err != nil {
			return err
		}
	}
	extMgr, stopExt := setupNonInteractiveExtensions(ctx, hostArgs, &r, "continuous")
	defer stopExt()
	store, err := openContinuousStore(ctx, opts.store, opts.backend, opts.durability.Durability)
	if err != nil {
		return err
	}
	rt, err := continuous.New(store)
	if err != nil {
		store.Close()
		return err
	}
	defer func() { retErr = errors.Join(retErr, rt.Close()) }()
	var host *continuous.Host
	service := func() *continuous.Service {
		if host == nil {
			return nil
		}
		return host.Service()
	}
	engine := newContinuousEngine(r, extMgr, rt, service)
	host, err = continuous.NewHost(rt, engine, continuous.HostOptions{Execution: continuous.ExecutionOptions{MaxTurns: opts.maxTurns, Compaction: continuousCompaction(r)}, Policy: opts.policy})
	if err != nil {
		return err
	}
	// Keep successful generations alive until host shutdown. Engines already
	// built by a request or tool call still own their original manager.
	var reloadMu sync.Mutex
	var stopGenerations []func()
	defer func() {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		for _, stop := range stopGenerations {
			stop()
		}
	}()
	reload := func(requestCtx context.Context) (continuous.Engine, error) {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		if err := requestCtx.Err(); err != nil {
			return nil, err
		}
		// Extension processes belong to the host, not to the client that
		// requested the reload. Disconnecting that client must not kill them.
		next, stop, err := loadContinuousEngine(ctx, hostArgs, rt, service)
		if err != nil {
			return nil, err
		}
		if err := requestCtx.Err(); err != nil {
			stop()
			return nil, err
		}
		stopGenerations = append(stopGenerations, stop)
		fmt.Fprintln(os.Stderr, "zot continuous host: extensions loaded, publishing engine generation")
		return next, nil
	}
	var ln net.Listener
	if opts.socket != "" {
		ln, err = continuous.ListenLocal(opts.socket)
		if err != nil {
			return fmt.Errorf("listen on local endpoint: %w", err)
		}
		if runtime.GOOS != "windows" {
			defer os.Remove(opts.socket)
		}
	} else {
		ln, err = net.Listen("tcp", opts.listen)
		if err != nil {
			return err
		}
		if opts.tlsCert != "" {
			cfg, err := serverTLSConfig(opts.tlsCert, opts.tlsKey, opts.tlsClientCA)
			if err != nil {
				ln.Close()
				return err
			}
			ln = tls.NewListener(ln, cfg)
		}
	}
	index, err := continuous.NewSearchIndex(ctx, rt)
	if err != nil {
		ln.Close()
		return fmt.Errorf("build search index: %w", err)
	}
	server := &continuous.HostServer{Host: host, Engine: engine, Tokens: tokens, Version: "continuous", Reload: reload, SearchIndex: index,
		DefaultConfig: continuous.AgentConfig{Provider: r.Provider, Model: r.Model, Reasoning: r.Reasoning}}
	plan, err := rt.RecoveryPreview(ctx)
	if err != nil {
		ln.Close()
		return err
	}
	fmt.Fprintf(os.Stderr, "zot continuous host: store %s, listening on %s, %d interrupted run(s), %d blocked by policy %s\n", opts.store, ln.Addr(), len(plan.Actions), plan.Blocked, opts.policy)
	for _, action := range plan.Actions {
		fmt.Fprintf(os.Stderr, "  run %s conversation %s: %s (%s)\n", action.RunID, action.ConversationID, action.Action, action.Detail)
	}
	if tokens == nil {
		fmt.Fprintln(os.Stderr, "  no token file: every local connection is admin")
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go func() { errCh <- host.Run(serveCtx) }()
	go func() { errCh <- server.Serve(serveCtx, ln) }()
	stopReload := onReloadSignal(serveCtx, func() {
		engine, err := reload(serveCtx)
		if err == nil {
			_, err = host.Reload(serveCtx, engine)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "zot continuous host: reload failed, previous generation stays active: %v\n", err)
		}
	})
	err = <-errCh
	cancel()
	stopReload()
	<-errCh
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// continuousAttachOptions are the flags of `zot continuous attach`.
type continuousAttachOptions struct {
	socket    string
	address   string
	tlsCA     string
	token     string
	tokenFile string
	workspace string
	id        string
	prompt    string
	json      bool
	follow    bool
}

func parseContinuousAttachArgs(args []string) (continuousAttachOptions, error) {
	var opts continuousAttachOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		value := func() (string, error) {
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "--socket":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.socket = v
		case "--address":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.address = v
		case "--tls-ca":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.tlsCA = v
		case "--token":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.token = v
		case "--token-file":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.tokenFile = v
		case "--workspace":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.workspace = v
		case "--conversation":
			v, err := value()
			if err != nil {
				return opts, err
			}
			opts.id = v
		case "--json":
			opts.json = true
		case "--follow":
			opts.follow = true
		default:
			if strings.HasPrefix(arg, "-") {
				return opts, fmt.Errorf("unknown attach flag: %s", arg)
			}
			if opts.prompt != "" {
				return opts, fmt.Errorf("attach accepts one prompt")
			}
			opts.prompt = arg
		}
	}
	if opts.socket == "" && opts.address == "" {
		return opts, fmt.Errorf("attach requires --socket or --address")
	}
	if opts.workspace == "" && opts.id == "" {
		return opts, fmt.Errorf("attach requires --workspace or --conversation")
	}
	if opts.prompt == "" && !opts.follow {
		return opts, fmt.Errorf("attach requires a prompt or --follow")
	}
	return opts, nil
}

// runContinuousAttach connects to a running host, submits an optional prompt
// to the workspace root (or a named conversation), and prints the answer.
// With --follow it keeps watching and printing new entries until interrupted.
// Closing the client never cancels host work.
func runContinuousAttach(ctx context.Context, args []string, out io.Writer) error {
	opts, err := parseContinuousAttachArgs(args)
	if err != nil {
		return err
	}
	token := opts.token
	if opts.tokenFile != "" {
		data, err := os.ReadFile(opts.tokenFile)
		if err != nil {
			return err
		}
		token = strings.Fields(strings.TrimSpace(string(data)))[0]
	}
	var conn net.Conn
	if opts.socket != "" {
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		conn, err = continuous.DialLocal(dialCtx, opts.socket)
		cancel()
	} else if opts.tlsCA != "" {
		conn, err = dialTLS(ctx, opts.address, opts.tlsCA)
	} else {
		dialer := &net.Dialer{Timeout: 5 * time.Second}
		conn, err = dialer.DialContext(ctx, "tcp", opts.address)
	}
	if err != nil {
		return fmt.Errorf("connect to host: %w", err)
	}
	defer conn.Close()
	// Cancellation must interrupt both protocol reads and writes, including
	// an idle watch or a submission waiting for approval on the host.
	stopDetach := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopDetach()
	client := &attachClient{conn: conn, reader: bufio.NewReader(conn)}
	if token != "" {
		if _, err := client.call(ctx, "hello", map[string]any{"token": token}); err != nil {
			return err
		}
	}
	id := opts.id
	if id == "" {
		data, err := client.call(ctx, "conversation.create", map[string]any{"workspace": opts.workspace})
		if err != nil {
			return err
		}
		var c continuous.Conversation
		json.Unmarshal(data, &c)
		id = c.ID
	}
	enc := json.NewEncoder(out)
	if opts.prompt != "" {
		data, err := client.call(ctx, "conversation.submit", map[string]any{"id": id, "content": opts.prompt, "request_id": randomRequestID()})
		if err != nil {
			return err
		}
		var sub continuous.Submission
		json.Unmarshal(data, &sub)
		data, err = client.call(ctx, "submission.wait", map[string]any{"id": sub.ID})
		if err != nil {
			return err
		}
		json.Unmarshal(data, &sub)
		snapData, err := client.call(ctx, "conversation.snapshot", map[string]any{"id": id, "limit": 5})
		if err != nil {
			return err
		}
		var snap continuous.ConversationSnapshot
		json.Unmarshal(snapData, &snap)
		if opts.json {
			enc.Encode(map[string]any{"type": "submission", "submission": sub, "conversation": id})
		} else {
			if sub.State != "answered" {
				return fmt.Errorf("submission %s", sub.State)
			}
			for i := len(snap.Entries) - 1; i >= 0; i-- {
				if snap.Entries[i].Type == "assistant" {
					fmt.Fprintln(out, snap.Entries[i].Content)
					break
				}
			}
		}
		if !opts.follow {
			return nil
		}
	}
	snapData, err := client.call(ctx, "conversation.snapshot", map[string]any{"id": id, "limit": 1})
	if err != nil {
		return err
	}
	var snap continuous.ConversationSnapshot
	json.Unmarshal(snapData, &snap)
	watchID := client.send("conversation.watch", map[string]any{"id": id, "after": snap.Revision})
	for {
		frame, err := client.read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if frame["type"] == "response" && frame["id"] == watchID && frame["success"] == false {
			return fmt.Errorf("watch: %v", frame["error"])
		}
		if frame["type"] != "commit" {
			continue
		}
		if opts.json {
			enc.Encode(frame)
			continue
		}
		commit, _ := frame["commit"].(map[string]any)
		ops, _ := commit["operations"].([]any)
		for _, op := range ops {
			m, _ := op.(map[string]any)
			key, _ := m["key"].(string)
			if !strings.HasPrefix(key, "entry/"+id+"/") {
				continue
			}
			raw, _ := json.Marshal(m["value"])
			var e continuous.Entry
			if json.Unmarshal(raw, &e) == nil && (e.Type == "assistant" || e.Type == "user") && e.Content != "" {
				fmt.Fprintf(out, "[%s] %s\n", e.Type, e.Content)
			}
		}
	}
}

// dialTLS connects to a TLS host, trusting only the given CA bundle.
func dialTLS(ctx context.Context, address, caFile string) (net.Conn, error) {
	cfg, err := continuous.ClientTLSConfig(caFile)
	if err != nil {
		return nil, err
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: cfg}
	return d.DialContext(ctx, "tcp", address)
}

type attachClient struct {
	conn   net.Conn
	reader *bufio.Reader
	next   int
}

func (c *attachClient) send(method string, params any) string {
	c.next++
	id := fmt.Sprint("a", c.next)
	b, _ := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	c.conn.Write(append(b, '\n'))
	return id
}

func (c *attachClient) read(ctx context.Context) (map[string]any, error) {
	if deadline, ok := ctx.Deadline(); ok {
		c.conn.SetReadDeadline(deadline)
	}
	line, err := c.reader.ReadBytes('\n')
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	var frame map[string]any
	if err := json.Unmarshal(line, &frame); err != nil {
		return nil, err
	}
	return frame, nil
}

func (c *attachClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.send(method, params)
	for {
		frame, err := c.read(ctx)
		if err != nil {
			return nil, err
		}
		if frame["id"] != id || frame["type"] != "response" {
			continue
		}
		if frame["success"] != true {
			return nil, fmt.Errorf("%s: %v (%v)", method, frame["error"], frame["code"])
		}
		data, _ := json.Marshal(frame["data"])
		return data, nil
	}
}

func randomRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprint("attach-", time.Now().UnixNano())
	}
	return "attach-" + hex.EncodeToString(b[:])
}
