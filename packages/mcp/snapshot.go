package mcp

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// State is a server's connection state as shown to users.
type State string

const (
	StateConnecting State = "connecting"
	StateConnected  State = "connected"
	StateFailed     State = "failed"
	StateDisabled   State = "disabled"
	StateInvalid    State = "invalid"
	// StateSignIn marks an OAuth server without a usable token.
	StateSignIn State = "sign-in required"
)

// ToolInfo describes one tool a server offers, using its remote name.
type ToolInfo struct {
	Name        string
	Description string
	Exposure    string // effective exposure after toolExposure
}

// ServerInfo is a display snapshot of one configured server. It never
// contains expanded environment values, so it is safe to render.
type ServerInfo struct {
	Name      string
	State     State
	Problem   string // short reason for failed or invalid servers
	Transport string // stdio, streamable-http, or sse
	Target    string // unexpanded command line or URL
	Exposure  string
	Source    string // file defining the effective entry
	Project   bool   // Source is the project file
	Override  bool   // project entry replaces a user entry
	ToolCount int
	Tools     []ToolInfo
	// OAuth reports that the server authenticates with OAuth, so sign-in
	// and sign-out apply. SignedIn reports a stored token.
	OAuth    bool
	SignedIn bool
	// ToolExposure is set when individual tools override Exposure.
	ToolExposure bool
	// Resources reports that the server offers MCP resources the model
	// can read.
	Resources bool
}

// Snapshot is the manager state the interactive UI renders.
type Snapshot struct {
	Loaded      bool
	UserPath    string
	ProjectPath string
	Servers     []ServerInfo
	FileErrors  []string
}

// Snapshot returns the current server list ordered by attention: failed
// and invalid first, then connecting, connected, and disabled, each by name.
func (m *Manager) Snapshot() Snapshot {
	// The token file is read once, before the lock, so rendering never
	// performs file I/O while holding the manager lock.
	credentials := m.tokens.snapshot()
	signedIn := func(name string, cfg ServerConfig) bool {
		entry, ok := credentials.Servers[credentialKey(name, cfg.URL)]
		return ok && entry.Token != nil && entry.URL == cfg.URL
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	cwd := m.loadedCWD
	if !m.loaded {
		cwd = m.cwd
	}
	snap := Snapshot{
		Loaded:      m.loaded,
		UserPath:    UserConfigPath(m.home),
		ProjectPath: ProjectConfigPath(cwd),
		FileErrors:  append([]string(nil), m.fileErrs...),
	}
	project := ProjectConfigPath(cwd)
	for name, s := range m.servers {
		info := ServerInfo{
			Name:         name,
			State:        StateConnected,
			Transport:    s.cfg.Transport,
			Target:       describeTarget(s.raw),
			Exposure:     s.cfg.Exposure,
			Source:       s.source,
			Project:      s.source == project,
			Override:     s.override,
			ToolCount:    s.count,
			Tools:        append([]ToolInfo(nil), s.tools...),
			OAuth:        oauthEnabled(s.cfg) && !s.disabled() && (s.cfg.OAuth != nil || errors.Is(s.err, ErrSignInRequired) || signedIn(name, s.cfg)),
			ToolExposure: len(s.cfg.ToolExposure) > 0,
			Resources:    s.resources && s.client != nil && s.err == nil && s.cfg.Exposure != "hidden",
		}
		switch {
		case s.cfg.Enabled != nil && !*s.cfg.Enabled:
			info.State = StateDisabled
			info.Exposure = exposureOf(s.raw)
			// A bare {"enabled": false} override has no transport to show.
			if s.raw.Command != "" || s.raw.URL != "" {
				info.Transport = transportOf(s.raw)
			}
		case s.connecting:
			info.State = StateConnecting
		case errors.Is(s.err, ErrSignInRequired):
			info.State = StateSignIn
			info.Problem = "sign in to connect"
		case s.err != nil:
			info.State = StateFailed
			info.Problem = describeFailure(s.err)
		case s.lost.Load():
			info.State = StateFailed
			info.Problem = "disconnected, reconnect to retry"
		}
		snap.Servers = append(snap.Servers, info)
	}
	for name, reason := range m.invalid {
		source := m.invalidSources[name]
		snap.Servers = append(snap.Servers, ServerInfo{
			Name: name, State: StateInvalid, Problem: reason, Source: source, Project: source == project,
		})
	}
	for idx := range snap.Servers {
		info := &snap.Servers[idx]
		if info.OAuth {
			if s := m.servers[info.Name]; s != nil {
				info.SignedIn = signedIn(info.Name, s.cfg)
			}
		}
	}
	sort.Slice(snap.Servers, func(i, j int) bool {
		a, b := snap.Servers[i], snap.Servers[j]
		if rank(a.State) != rank(b.State) {
			return rank(a.State) < rank(b.State)
		}
		return a.Name < b.Name
	})
	return snap
}

func rank(s State) int {
	switch s {
	case StateFailed, StateInvalid, StateSignIn:
		return 0
	case StateConnecting:
		return 1
	case StateConnected:
		return 2
	}
	return 3
}

func describeTarget(c ServerConfig) string {
	if c.URL != "" {
		return redactURL(c.URL)
	}
	if c.Command == "" {
		return ""
	}
	parts := []string{quoteWord(c.Command)}
	for _, a := range c.Args {
		parts = append(parts, quoteWord(a))
	}
	return strings.Join(parts, " ")
}

// redactURL hides literal credentials in user info and query values.
// ${VARIABLE} references are kept, since they are not secrets.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	if u.User != nil {
		u.User = url.User("***")
	}
	query := u.Query()
	for key, values := range query {
		for idx, value := range values {
			if !envReference.MatchString(value) || envReference.ReplaceAllString(value, "") != "" {
				values[idx] = "***"
			}
		}
		query[key] = values
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	out := u.String()
	// Encoding escapes the braces of kept references, restore them.
	return strings.NewReplacer("%24%7B", "${", "%7D", "}", "%2A", "*").Replace(out)
}

func quoteWord(s string) string {
	if s == "" || strings.ContainsAny(s, " \t\"'\\") {
		return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
	}
	return s
}

func transportOf(c ServerConfig) string {
	t := c.Transport
	if t == "" {
		t = c.Type
	}
	switch {
	case t == "http":
		return "streamable-http"
	case t != "":
		return t
	case c.URL != "":
		return "streamable-http"
	}
	return "stdio"
}

func exposureOf(c ServerConfig) string {
	if c.Exposure == "" {
		return "deferred"
	}
	return c.Exposure
}

// Match explicit HTTP status labels, never numeric fragments of URLs or ports.
var failureHTTPStatus = regexp.MustCompile(`\b(?:status(?: code)?[: ]+|http(?:/[0-9.]+)? +)(401|403|404)\b`)

// describeFailure maps a connection error to a short, credential-free hint.
// Raw transport errors can contain URLs, headers, or arguments.
func describeFailure(err error) string {
	text := strings.ToLower(err.Error())
	status := ""
	if match := failureHTTPStatus.FindStringSubmatch(text); match != nil {
		status = match[1]
	}
	switch {
	case strings.Contains(text, "legacy sse"):
		return "endpoint rejected streamable HTTP, try transport sse or check the URL"
	case status == "401" || strings.Contains(text, "unauthorized") || strings.Contains(text, "authorization required"):
		return "server rejected the credentials (401)"
	case status == "403" || strings.Contains(text, "forbidden"):
		return "access forbidden (403)"
	case status == "404":
		return "endpoint not found (404)"
	case strings.Contains(text, "chdir"):
		return "working directory not found"
	case strings.Contains(text, "executable file not found") || strings.Contains(text, "no such file"):
		return "command not found"
	case strings.Contains(text, "permission denied"):
		return "command is not executable"
	case strings.Contains(text, "deadline exceeded") || strings.Contains(text, "timeout"):
		return "timed out while connecting"
	case strings.Contains(text, "connection refused"):
		return "connection refused"
	case strings.Contains(text, "no such host"):
		return "host not found"
	case strings.Contains(text, "collide"):
		return "tool names collide after normalization"
	}
	return "connection or tool discovery failed"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}
