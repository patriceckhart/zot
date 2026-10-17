package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type serverTemplate struct {
	Name        string
	Description string
	Config      func(cwd string) ServerConfig
}

func setupTemplates() map[string]serverTemplate {
	return map[string]serverTemplate{
		"grep": {
			Name:        "grep",
			Description: "Search real-world code across public GitHub repositories via grep.app.",
			Config: func(cwd string) ServerConfig {
				return ServerConfig{
					Transport: "streamable-http",
					URL:       "https://mcp.grep.app/",
				}
			},
		},
		"filesystem": {
			Name:        "filesystem",
			Description: "Read/write files under the current project directory using the official filesystem MCP server.",
			Config: func(cwd string) ServerConfig {
				return ServerConfig{
					Transport: "stdio",
					Command:   "npx",
					Args:      []string{"-y", "@modelcontextprotocol/server-filesystem", cwd},
				}
			},
		},
		"context7": {
			Name:        "context7",
			Description: "Fetch up-to-date library documentation and examples via Context7.",
			Config: func(cwd string) ServerConfig {
				return ServerConfig{
					Transport: "stdio",
					Command:   "npx",
					Args:      []string{"-y", "@upstash/context7-mcp@latest"},
				}
			},
		},
		"playwright": {
			Name:        "playwright",
			Description: "Browser automation via Playwright MCP.",
			Config: func(cwd string) ServerConfig {
				return ServerConfig{
					Transport: "stdio",
					Command:   "npx",
					Args:      []string{"-y", "@executeautomation/playwright-mcp-server"},
				}
			},
		},
		"you": {
			Name:        "you",
			Description: "Current web search via the keyless You.com MCP server (you-search). Add an Authorization Bearer header with a YDC_API_KEY for additional authenticated tools.",
			Config: func(cwd string) ServerConfig {
				return ServerConfig{
					Transport: "streamable-http",
					URL:       "https://api.you.com/mcp?profile=free",
				}
			},
		},
		"serply": {
			Name:        "serply",
			Description: "Google-backed search via the Serply MCP server: web, news, and scholar results with citation counts, plus maps, jobs, video, Bing, Amazon products, Reddit, and URL scraping. Requires a SERPLY_API_KEY.",
			Config: func(cwd string) ServerConfig {
				return ServerConfig{
					Transport: "streamable-http",
					URL:       "https://api.serply.io/mcp",
					Headers:   map[string]string{"X-Api-Key": "${SERPLY_API_KEY}"},
				}
			},
		},
		"cohesivity": {
			Name:        "cohesivity",
			Description: "Backend services for the app being built via the keyless Cohesivity MCP server: Postgres, Redis, object storage, hosting, login, and an email inbox.",
			Config: func(cwd string) ServerConfig {
				return ServerConfig{
					Transport: "streamable-http",
					URL:       "https://cohesivity.ai/mcp",
					Headers:   map[string]string{"User-Agent": "zot-mcp-bridge"},
				}
			},
		},
	}
}

func setupHelp() string {
	templates := setupTemplates()
	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("mcp-bridge setup\n\n")
	b.WriteString("Add a known MCP server template to your zot MCP config.\n\n")
	b.WriteString("Usage:\n")
	b.WriteString("  /mcp setup add <template> [--global|--project] [--name <server-name>]\n")
	b.WriteString("  /mcp setup templates\n\n")
	b.WriteString("Templates:\n")
	for _, name := range names {
		t := templates[name]
		b.WriteString(fmt.Sprintf("  %-12s %s\n", t.Name, t.Description))
	}
	b.WriteString("\nExamples:\n")
	b.WriteString("  /mcp setup add grep\n")
	b.WriteString("  /mcp setup add filesystem --project\n")
	b.WriteString("  /mcp setup add context7 --global --name docs\n")
	b.WriteString("\nDefault target is global: $ZOT_HOME/mcp.json. Run /reload-ext after changes.\n")
	return b.String()
}

func handleSetup(args []string, cwd string) (string, error) {
	if len(args) == 0 || args[0] == "help" {
		return setupHelp(), nil
	}
	if args[0] == "templates" || args[0] == "list" {
		return setupHelp(), nil
	}
	if args[0] != "add" {
		return "", fmt.Errorf("unknown setup command %q\n\n%s", args[0], setupHelp())
	}
	if len(args) < 2 {
		return "", fmt.Errorf("usage: /mcp setup add <template> [--global|--project] [--name <server-name>]")
	}

	templateName := args[1]
	templates := setupTemplates()
	t, ok := templates[templateName]
	if !ok {
		return "", fmt.Errorf("unknown template %q\n\n%s", templateName, setupHelp())
	}

	target := "global"
	serverName := templateName
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--global":
			target = "global"
		case "--project":
			target = "project"
		case "--name":
			if i+1 >= len(args) {
				return "", fmt.Errorf("--name requires a value")
			}
			serverName = args[i+1]
			i++
		default:
			return "", fmt.Errorf("unknown setup option %q", args[i])
		}
	}

	path := filepath.Join(zotHome(), "mcp.json")
	if target == "project" {
		if cwd == "" {
			return "", fmt.Errorf("--project requires a working directory, but none is known")
		}
		path = filepath.Join(cwd, ".zot", "mcp.json")
	}

	cfg, err := readConfigFile(path)
	if err != nil {
		return "", err
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ServerConfig{}
	}
	if _, exists := cfg.MCPServers[serverName]; exists {
		return "", fmt.Errorf("server %q already exists in %s", serverName, path)
	}

	cfg.MCPServers[serverName] = t.Config(cwd)
	if err := writeConfigFile(path, cfg); err != nil {
		return "", err
	}

	return fmt.Sprintf("Added MCP server %q from template %q to %s.\n\nRun /reload-ext to reload MCP tools.", serverName, templateName, path), nil
}

func readConfigFile(path string) (Config, error) {
	cfg := Config{MCPServers: map[string]ServerConfig{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ServerConfig{}
	}
	return cfg, nil
}

func writeConfigFile(path string, cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// 0o600: mcp.json can contain auth headers / tokens.
	return writeFileAtomic(path, data, 0o600)
}
