// mcp-bridge — Connect zot to MCP (Model Context Protocol) servers.
//
// This extension reads MCP server configurations from standard locations
// (same format as Claude Desktop, Cursor, etc.) and bridges their tools
// into zot so the LLM can call them.
//
// Config locations:
//   - Global:  $ZOT_HOME/mcp.json
//   - Project: .zot/mcp.json
//
// Smart lazy: servers are spawned on startup to discover tools, then
// killed after 5 minutes of idle time. On the next tool call, they're
// respawned automatically.
//
// Tool naming: mcp__<server>__<tool>
//
// Slash commands:
//
//	/mcp              — show status of all configured servers
//	/mcp start <name> — manually start a server
//	/mcp stop <name>  — manually stop a server
//	/mcp restart      — restart all servers
//	/mcp start all    — manually start all servers
//	/mcp stop all     — manually stop all servers
//
// Build:
//
//	cd examples/extensions/mcp-bridge
//	go build -o mcp-bridge .
//
// Install:
//
//	zot ext install .
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/agent/ext"
)

func main() {
	e := ext.New("mcp", "1.1.0")

	// Logger writes to stderr (captured by zot into ext logs)
	logger := log.New(os.Stderr, "[mcp-bridge] ", log.LstdFlags)

	var b *bridge
	e.OnHello(func(host ext.HostInfo) {
		cwd := host.CWD
		if cwd == "" {
			var err error
			cwd, err = os.Getwd()
			if err != nil {
				logger.Printf("getwd fallback: %v", err)
			}
		}

		cfg, err := loadConfig(cwd)
		if err != nil {
			logger.Printf("config error: %v", err)
			notifyText(e, "error", "mcp: config error: "+err.Error())
		}

		if len(cfg.MCPServers) == 0 {
			logger.Printf("no MCP servers configured")
			// Still register commands so user can check status and run setup.
			registerCommands(e, nil)
			return
		}

		logger.Printf("found %d MCP server(s)", len(cfg.MCPServers))

		b = newBridge(e, cwd, logger)
		b.loadServers(cfg)
		b.registerToolSearch()

		cachePath := toolCachePath()
		cache, err := readToolCache(cachePath)
		if err != nil {
			logger.Printf("tool cache error: %v", err)
		}
		cachedToolCount := b.registerCachedTools(cache)
		logger.Printf("registered %d cached MCP tool(s)", cachedToolCount)

		b.startIdleReaper()
		registerCommands(e, b)
		startBackgroundToolRefresh(e, b, logger, cachePath, cachedToolCount)
	})

	// Run the extension protocol loop
	if err := e.Run(); err != nil {
		logger.Printf("fatal: %v", err)
	}

	// Cleanup
	if b != nil {
		b.stopAll()
	}
}

func startBackgroundToolRefresh(e *ext.Extension, b *bridge, logger *log.Logger, cachePath string, cachedToolCount int) {
	// Refresh discovery in the background so zot startup is not blocked. Only ask
	// for /reload-ext if the discovered tool cache actually changed; otherwise a
	// reload would just repeat this cycle without adding anything.
	go func() {
		time.Sleep(500 * time.Millisecond) // Let ready/registrations flush first.
		if cachedToolCount == 0 {
			notifyText(e, "warn", "MCP loaded without cached tools. Refreshing tool cache in background.")
		} else {
			notifyText(e, "success", fmt.Sprintf("MCP loaded %d cached tool(s). Refreshing cache in background.", cachedToolCount))
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		changed, err := b.refreshToolCache(ctx, cachePath)
		if err != nil {
			logger.Printf("tool cache refresh error: %v", err)
			notifyText(e, "warn", "MCP tool cache refresh partially failed: "+err.Error())
			return
		}
		if changed {
			notifyText(e, "success", "MCP tool cache changed. Run /reload-ext once to load the updated tools.")
			return
		}
		notifyBridgeStatus(e, b)
	}()
}

// registerCommands sets up the /mcp slash commands.
func registerCommands(e *ext.Extension, b *bridge) {
	e.Command("mcp", "show MCP server status or manage servers", func(args string) ext.Response {
		args = strings.TrimSpace(args)

		// Parse subcommand
		parts := strings.Fields(args)
		if len(parts) == 0 {
			// /mcp — show a compact dashboard: status plus discoverable actions.
			notifyText(e, "info", mcpOverview(b))
			return ext.Noop()
		}

		switch parts[0] {
		case "help", "--help", "-h":
			notifyText(e, "info", mcpHelp(b))
			return ext.Noop()

		case "setup":
			out, err := handleSetup(parts[1:], e.Host().CWD)
			if err != nil {
				return ext.Errorf("%v", err)
			}
			notifyText(e, "info", out)
			return ext.Noop()

		case "login", "logout":
			if b == nil || len(parts) != 2 { return ext.Errorf("usage: /mcp %s <server>", parts[0]) }
			srv, ok := b.servers[parts[1]]
			if !ok { return ext.Errorf("unknown server: %s", parts[1]) }
			if parts[0] == "logout" {
				srv.stop()
				if err := os.Remove(oauthStoreFor(srv.config.URL).path); err != nil && !os.IsNotExist(err) { return ext.Errorf("OAuth logout failed") }
				notifyText(e, "info", "Local OAuth credentials removed (server-side grant is not revoked).")
				return ext.Noop()
			}
			if err := srv.login(context.Background(), func(u string) { notifyText(e, "info", "Open this URL in your browser to authorize MCP access:\n"+u) }); err != nil { return ext.Errorf("OAuth login: %v", err) }
			srv.stop()
			notifyText(e, "info", "OAuth credentials saved. Run /mcp refresh to reconnect and discover tools.")
			return ext.Noop()

		case "start":
			return handleStartCommand(e, b, parts[1:])

		case "stop":
			return handleStopCommand(e, b, parts[1:])

		case "restart":
			return handleRestartCommand(e, b)

		case "refresh", "discover":
			return handleRefreshCommand(e, b, toolCachePath())

		default:
			// /mcp <name> — show detailed status for one server
			if b == nil {
				return ext.Errorf("no servers configured")
			}
			name := parts[0]
			srv, ok := b.servers[name]
			if !ok {
				return ext.Errorf("unknown server: %s", name)
			}
			notifyText(e, serverNotifyLevel(srv), srv.detailStatus())
			return ext.Noop()
		}
	})

}

func mcpOverview(b *bridge) string {
	var sb strings.Builder
	sb.WriteString("MCP status\n")
	if b == nil || len(b.servers) == 0 {
		sb.WriteString("  no servers configured\n")
	} else {
		for _, line := range b.serverStatus() {
			sb.WriteString("  ")
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
	}
	sb.WriteString("\nCommon commands\n")
	sb.WriteString("  /mcp <server>                         Show detailed status for one server\n")
	sb.WriteString("  /mcp start <server|all>               Start one server, or all servers\n")
	sb.WriteString("  /mcp stop <server|all>                Stop one server, or all servers\n")
	sb.WriteString("  /mcp restart                          Restart all servers\n")
	sb.WriteString("  /mcp login <server>                   Authorize in your browser (OAuth + PKCE)\n")
	sb.WriteString("  /mcp logout <server>                  Remove local OAuth credentials\n")
	sb.WriteString("  /mcp refresh                          Refresh cached tool definitions\n")
	sb.WriteString("  /mcp help                             Show all MCP commands\n")
	return strings.TrimRight(sb.String(), "\n")
}

func mcpHelp(b *bridge) string {
	var sb strings.Builder
	sb.WriteString(mcpOverview(b))
	sb.WriteString("\n\nSetup commands\n")
	sb.WriteString("  /mcp setup templates                  List setup templates\n")
	sb.WriteString("  /mcp setup add <template> [options]   Add a server template\n")
	sb.WriteString("\nSetup options\n")
	sb.WriteString("  --global                              Write to $ZOT_HOME/mcp.json (default)\n")
	sb.WriteString("  --project                             Write to .zot/mcp.json\n")
	sb.WriteString("  --name <server-name>                  Use a custom configured server name\n")
	return strings.TrimRight(sb.String(), "\n")
}

func handleStartCommand(e *ext.Extension, b *bridge, args []string) ext.Response {
	if len(args) != 1 {
		return ext.Errorf("usage: /mcp start <server-name|all>")
	}
	if b == nil {
		return ext.Errorf("no servers configured")
	}
	name := args[0]
	if name == "all" {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := b.startAll(ctx); err != nil {
			return ext.Errorf("start all failed: %v", err)
		}
		notifyBridgeStatus(e, b)
		return ext.Noop()
	}
	if err := b.startServer(name); err != nil {
		return ext.Errorf("start %s: %v", name, err)
	}
	notifyBridgeStatus(e, b)
	return ext.Noop()
}

func handleStopCommand(e *ext.Extension, b *bridge, args []string) ext.Response {
	if len(args) != 1 {
		return ext.Errorf("usage: /mcp stop <server-name|all>")
	}
	if b == nil {
		return ext.Errorf("no servers configured")
	}
	name := args[0]
	if name == "all" {
		b.stopAll()
		notifyBridgeStatus(e, b)
		return ext.Noop()
	}
	if err := b.stopServer(name); err != nil {
		return ext.Errorf("stop %s: %v", name, err)
	}
	notifyBridgeStatus(e, b)
	return ext.Noop()
}

func handleRestartCommand(e *ext.Extension, b *bridge) ext.Response {
	if b == nil {
		return ext.Errorf("no servers configured")
	}
	b.stopAll()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.startAll(ctx); err != nil {
		return ext.Errorf("restart failed: %v", err)
	}
	notifyBridgeStatus(e, b)
	return ext.Noop()
}

func handleRefreshCommand(e *ext.Extension, b *bridge, cachePath string) ext.Response {
	if b == nil {
		return ext.Errorf("no servers configured")
	}
	notifyText(e, "info", "Refreshing MCP tool cache…")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		changed, err := b.refreshToolCache(ctx, cachePath)
		if err != nil {
			notifyText(e, "warn", "MCP tool cache refresh partially failed: "+err.Error())
			return
		}
		if changed {
			notifyText(e, "success", "MCP tool cache changed. Run /reload-ext once to load the updated tools.")
			return
		}
		notifyBridgeStatus(e, b)
	}()
	return ext.Noop()
}

func notifyBridgeStatus(e *ext.Extension, b *bridge) {
	if e == nil || b == nil {
		return
	}
	notifyText(e, b.notifyLevel(), formatStatusSummary(b))
}

func serverNotifyLevel(srv *managedServer) string {
	if srv == nil {
		return "info"
	}
	srv.mu.Lock()
	state := srv.state
	srv.mu.Unlock()
	switch state {
	case stateReady:
		return "success"
	case stateError:
		return "error"
	default:
		return "info"
	}
}

func notifyText(e *ext.Extension, level, text string) {
	if e == nil {
		return
	}
	clearExtensionNotes()
	e.Notify(level, text)
}

func clearExtensionNotes() {
	_, _ = os.Stdout.WriteString("{\"type\":\"clear_notes\"}\n")
}

// formatStatusSummary builds a human-readable status line.
func formatStatusSummary(b *bridge) string {
	lines := b.serverStatus()
	if len(lines) == 0 {
		return "no MCP servers"
	}
	return strings.Join(lines, " | ")
}
