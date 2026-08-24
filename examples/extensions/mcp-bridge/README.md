# mcp-bridge

Connect zot to [MCP (Model Context Protocol)](https://modelcontextprotocol.io) servers.

This extension reads MCP server configurations from standard locations (same format as Claude Desktop, Cursor, Cline, etc.) and bridges their tools into zot so the LLM can call them directly.

## Features

- **Standard config format** — same JSON as Claude Desktop, Cursor, Cline
- **On-demand tool discovery** — only `mcp__search_tools` is advertised initially; matching MCP schemas load when needed instead of bloating every model request
- **Smart lazy loading** — cached definitions register as deferred tools at startup, servers wake for refresh or tool calls, then auto-sleep after idle time
- **Auto-respawn** — calling a loaded tool on a sleeping server wakes it up automatically
- **Multi-transport** — stdio, streamable-http, and SSE transports
- **Multi-server** — connect to any number of MCP servers simultaneously
- **Tool namespacing** — tools appear as `mcp__<server>__<tool>` to avoid collisions
- **Tool annotations** — read-only, destructive, idempotent hints surfaced to LLM
- **Configurable timeouts** — per-server connect, request, and idle timeouts
- **Custom headers** — auth tokens and other headers for HTTP servers
- **Slash commands** — `/mcp` to check status, start/stop/restart servers
- **Better error messages** — context-aware errors with actionable suggestions

## Quick Start

1. **Build the extension:**

   ```bash
   cd examples/extensions/mcp-bridge
   go build -o mcp-bridge .
   ```

2. **Create a project config file:**

   ```bash
   mkdir -p .zot
   cat > .zot/mcp.json << 'EOF'
   {
     "mcpServers": {
       "filesystem": {
         "command": "npx",
         "args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
       },
       "context7": {
         "command": "npx",
         "args": ["-y", "@upstash/context7-mcp@latest"]
       }
     }
   }
   EOF
   ```

3. **Install the extension:**

   ```bash
   zot ext install .
   ```

4. **Restart zot.** On first run the extension refreshes its tool cache in the background. When zot shows `MCP tool cache changed`, run `/reload-ext` once. Future launches register the cached MCP tools immediately as deferred definitions.

The model initially sees one small loader tool, `mcp__search_tools`. It searches cached MCP tool names and descriptions locally, activates up to eight relevant definitions by default, and then calls the selected MCP tool normally. This keeps large MCP installations compatible with providers that limit request or tool-schema size.

### Phone pairing with Build Remote Agent

[Build Remote Agent](https://grokbuildremote.com/) is an optional, third-party integration that lets a paired phone observe terminal sessions and veto actions. It is an independent Linespotting AB product and is not affiliated with xAI or SpaceX.

#### Install the agent

Pin release `v0.6.0` and verify the binary against the checksum listed here. The release's own `SHA256SUMS` file does not currently match five of its six binary assets, so do not use that file as the trust source. These checksums were verified directly against the release assets on 2026-08-24:

```text
62673a6856342a87d4a2a659bc1de92200aa19a5b60d88d252254940820f0b7f  gbr-agent-darwin-amd64
7baa1a8e214cd71b60e3f2b5063713e00ff740939749c3cab3d702784a1432f8  gbr-agent-darwin-arm64
fb54724367882497f2e8e05e40ecdeb4be29e008e6c865fc5c426cf464e6ad6e  gbr-agent-linux-amd64
9e9d7ca45bb0c4ded9d04226136013e9b64ae30f16bcf03069d35e9c38171cb9  gbr-agent-linux-arm64
40355b2be6cd68f3be68f2a06dfd30307ec1a60f16f87f1d6174012b35aa4a49  gbr-agent-windows-amd64.exe
8fb9efcbc7e2ac91c11964944bf0f45e31bb23f4356d9dcb4b305d7cb9b0fe8c  gbr-agent-windows-arm64.exe
```

This macOS Apple Silicon example downloads, verifies, and installs the binary. Change both `ASSET` and `SHA` for another platform.

```bash
VER=v0.6.0
ASSET=gbr-agent-darwin-arm64
SHA=7baa1a8e214cd71b60e3f2b5063713e00ff740939749c3cab3d702784a1432f8
BASE="https://github.com/LinespottingOrg/GrokBuildRemote-Agents/releases/download/$VER"
curl -fsSL -o "$ASSET" "$BASE/$ASSET"
if command -v sha256sum >/dev/null 2>&1; then
  printf '%s  %s\n' "$SHA" "$ASSET" | sha256sum -c -
else
  printf '%s  %s\n' "$SHA" "$ASSET" | shasum -a 256 -c -
fi
mkdir -p "$HOME/.local/bin"
install -m 0755 "$ASSET" "$HOME/.local/bin/gbr-agent"
export PATH="$HOME/.local/bin:$PATH"
gbr-agent version  # must report v0.6.0
gbr-agent pair
gbr-agent run
```

Keep `gbr-agent run` running. Its Bot API should only be available over loopback:

```bash
curl -sS http://127.0.0.1:8788/health
curl -sS http://127.0.0.1:8788/v1/sessions
```

#### Install the MCP server

The MCP server requires Node.js 20 or newer. Pin its source instead of cloning the mutable default branch:

```bash
git clone --branch v0.6.0 --depth 1 https://github.com/LinespottingOrg/GrokBuildRemote-Agents.git
cd GrokBuildRemote-Agents/mcp/gbr-mcp
npm install --ignore-scripts
MCP_PATH="$(pwd)/bin/gbr-mcp.js"
node "$MCP_PATH" --diagnose
```

The `v0.6.0` source does not include a package lock, so its npm dependency resolution is not fully reproducible. Review the package manifest and resolved dependency tree before use. Put the absolute value of `MCP_PATH` in the configuration below. Never put mailbox keys in `mcp.json`.

## Configuration

Config files are loaded from two locations (project overrides global per-server):

| Location | Scope |
|---|---|
| `$ZOT_HOME/mcp.json` | Global (`$XDG_STATE_HOME/zot/mcp.json` when `XDG_STATE_HOME` is set) |
| `.zot/mcp.json` | Project-level (in your project root) |

### Config Format

Standard MCP config — same as Claude Desktop, with zot-specific extensions:

```jsonc
{
  "mcpServers": {
    // ── Stdio transport (local subprocess) ───────────────────────────────────
    "filesystem": {
      "command": "npx",                    // executable to spawn
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"],
      "env": {                             // extra environment variables
        "NODE_ENV": "production"
      },
      "connectTimeout": 30,                // connection timeout (seconds)
      "requestTimeout": 60,                // per-request timeout (seconds)
      "idleTimeout": 300                   // idle timeout before stopping (seconds)
    },

    // Build Remote Agent (gbr/1). Pair with `gbr-agent pair`, then keep
    // `gbr-agent run` running. Use an absolute path to the pinned MCP checkout.
    "gbr": {
      "command": "node",
      "args": ["/ABSOLUTE/PATH/GrokBuildRemote-Agents/mcp/gbr-mcp/bin/gbr-mcp.js"]
    },

    // ── Streamable HTTP transport (modern HTTP) ─────────────────────────────
    "supabase": {
      "transport": "streamable-http",
      "url": "https://mcp.supabase.com/mcp",
      "headers": {                         // custom HTTP headers
        "Authorization": "Bearer YOUR_TOKEN"
      }
    },

    // ── SSE transport (legacy HTTP) ─────────────────────────────────────────
    "legacy-server": {
      "transport": "sse",
      "url": "https://example.com/sse"
    }
  }
}
```

### Configuration Options

| Field | Type | Default | Description |
|---|---|---|---|
| `command` | string | — | Executable to spawn (stdio only) |
| `args` | string[] | [] | Arguments for the command |
| `env` | object | — | Extra environment variables |
| `transport` | string | "stdio" | Transport: "stdio", "streamable-http", or "sse" |
| `url` | string | — | Server URL (HTTP transports only) |
| `headers` | object | — | Custom HTTP headers (HTTP transports only) |
| `connectTimeout` | number | 30 | Connection timeout in seconds |
| `requestTimeout` | number | 60 | Per-request timeout in seconds |
| `idleTimeout` | number | 300 | Idle timeout before stopping in seconds |

### Example: Multiple Servers

```jsonc
{
  "mcpServers": {
    // Filesystem access (stdio)
    "filesystem": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-filesystem", "/home/user/projects"]
    },

    // Grep.app - Search GitHub (streamable-http)
    "grep": {
      "transport": "streamable-http",
      "url": "https://mcp.grep.app/"
    },

    // Database queries (stdio)
    "sqlite": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-sqlite", "test.db"]
    },

    // Documentation lookup (stdio)
    "context7": {
      "command": "npx",
      "args": ["-y", "@upstash/context7-mcp@latest"]
    }
  }
}
```

## How It Works

```
┌──────────────────────────────────────────────────────────────┐
│  zot agent                                                    │
│                                                               │
│  ┌──────────┐    tool_call    ┌──────────────┐               │
│  │   LLM    │───────────────▶│  mcp-bridge  │               │
│  │          │◀───────────────│  (extension) │               │
│  └──────────┘    tool_result └──────┬───────┘               │
│                                      │                        │
│                           ┌──────────┼──────────┐            │
│                           ▼          ▼          ▼            │
│                    ┌──────────┐ ┌──────────┐ ┌──────────┐   │
│                    │   MCP    │ │   MCP    │ │   MCP    │   │
│                    │ server 1 │ │ server 2 │ │ server 3 │   │
│                    │ (stdio)  │ │ (stdio)  │ │ (stdio)  │   │
│                    └──────────┘ └──────────┘ └──────────┘   │
└──────────────────────────────────────────────────────────────┘
```

1. **Startup**: mcp-bridge reads config and registers tools from `mcp-tools-cache.json`
2. **Background refresh**: starts configured MCP servers, calls `tools/list`, and updates the cache when definitions change
3. **Reload**: if the cache changed, run `/reload-ext` once so zot rebuilds the tool registry with the new definitions
4. **Naming**: tools appear as `mcp__<server>__<tool>` (e.g., `mcp__filesystem__read_file`)
5. **Idle timeout**: servers not used for 5 minutes are automatically stopped
6. **Auto-respawn**: calling a tool on a stopped server wakes it up
7. **Routing**: tool calls are forwarded to the appropriate MCP server

## Slash Commands

| Command | Description |
|---|---|
| `/mcp` | Show status of all configured servers |
| `/mcp help` | Show available MCP commands |
| `/mcp <name>` | Show detailed status for one server |
| `/mcp start <name>` | Manually start a server |
| `/mcp stop <name>` | Manually stop a server |
| `/mcp restart` | Restart all servers |
| `/mcp start all` | Manually start all servers |
| `/mcp stop all` | Manually stop all servers |
| `/mcp setup templates` | Show available setup templates |
| `/mcp setup add <template> [--global|--project] [--name <server-name>]` | Add a server from a template |

## Tool Naming

Tools are namespaced to avoid collisions with zot's built-in tools:

```
mcp__<server>__<tool>
```

Examples:
- `mcp__filesystem__read_file`
- `mcp__filesystem__write_file`
- `mcp__sqlite__query`
- `mcp__context7__resolve-library-id`

Server and tool names are sanitized (non-alphanumeric characters become `_`).

## Smart Lazy Loading

The bridge uses a "smart lazy" strategy:

1. **On startup**: cached tool definitions are registered without blocking zot startup
2. **In the background**: servers start long enough to refresh the tool cache
3. **During use**: servers stay running for fast tool calls
4. **After 5 min idle**: unused servers are automatically stopped (saves memory/CPU)
5. **On next tool call**: the server is respawned automatically (~1-3s delay)

This gives you:
- Cached tools visible to the LLM immediately
- Fast tool calls when actively working
- Memory freed when not using MCP tools
- One manual `/reload-ext` only when tool definitions change

## Troubleshooting

**Check server status:**
```
/mcp
```

**View extension logs:**
```bash
zot ext logs mcp -f
```

**Common issues:**

- **Server fails to start**: check that `command` exists in your PATH, or use absolute path
- **Tool not found**: run `/mcp` to see if the server started successfully
- **Slow first call**: server is respawning after idle timeout (normal)

## Limitations

- **No OAuth flow** — authentication requires manual token configuration in headers
- **No resources/prompts** — only tools are bridged (MCP resources and prompts coming later)
- **No automatic config hot reload** — run `/reload-ext` after setup/config changes

## Development

```bash
# Build
cd examples/extensions/mcp-bridge
go build -o mcp-bridge .

# Test
go test ./...
go vet ./...

# Run without installing (for one zot session)
zot --ext .

# View logs
zot ext logs mcp -f
```

## License

MIT

## Testing

```bash
go test ./...
go vet ./...
go build -o /tmp/mcp-bridge .
```

Tested MCP servers:

| Server | Transport | Result |
|---|---|---|
| `@modelcontextprotocol/server-filesystem` | stdio | 14 tools registered; file operations and MCP errors handled correctly |
| grep.app `https://mcp.grep.app/` | streamable-http | `searchGitHub` registered and successfully searched public GitHub code |

Note: grep.app uses the root endpoint `/`. Streamable HTTP protocol headers are handled automatically by the bridge and should not be written to `mcp.json`.

## What the paired phone sees

Build Remote Agent exposes terminal windows on the machine. Its loopback `:8788` endpoint is a Bot API that returns JSON, not an MCP endpoint or terminal transcript. See the third-party's [current phone visibility documentation](https://github.com/LinespottingOrg/GrokBuildRemote-Agents/blob/main/docs/WHAT-THE-PHONE-SEES.md) for details.
