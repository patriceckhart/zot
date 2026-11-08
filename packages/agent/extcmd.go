package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/patriceckhart/zot/internal/ignore"
	"github.com/patriceckhart/zot/packages/agent/extensions"
	"github.com/patriceckhart/zot/packages/agent/extproto"
)

// runExtCommand dispatches `zot ext ...` subcommands. Returns
// (handled=true, err) if rawArgs starts with "ext"; otherwise
// (handled=false, nil) so the main router falls through to the
// regular flag parser.
func runExtCommand(rawArgs []string, version string) (handled bool, err error) {
	if len(rawArgs) == 0 || rawArgs[0] != "ext" {
		return false, nil
	}
	if len(rawArgs) == 1 {
		printExtHelp()
		return true, nil
	}
	switch rawArgs[1] {
	case "list":
		return true, extList()
	case "doctor":
		return true, extDoctor(version)
	case "logs":
		return true, extLogs(rawArgs[2:])
	case "enable":
		return true, extToggle(rawArgs[2:], true)
	case "disable":
		return true, extToggle(rawArgs[2:], false)
	case "remove", "rm":
		return true, extRemove(rawArgs[2:])
	case "install":
		return true, extInstall(rawArgs[2:])
	case "help", "-h", "--help":
		printExtHelp()
		return true, nil
	default:
		printExtHelp()
		return true, fmt.Errorf("unknown ext subcommand: %s", rawArgs[1])
	}
}

func printExtHelp() {
	fmt.Fprintln(os.Stderr, `zot ext — manage extensions

usage:
  zot ext list                    list installed extensions and their state
  zot ext doctor                  diagnose installed extensions
  zot ext logs <name> [-f]        cat / tail an extension's stderr log
  zot ext enable <name>           re-enable a disabled extension
  zot ext disable <name>          disable without removing
  zot ext remove <name>           delete an extension directory
  zot ext install <path|git-url>  copy / clone an extension into $ZOT_HOME/extensions/

extensions live under:
  $ZOT_HOME/extensions/<name>/extension.json   (global)
  ./.zot/extensions/<name>/extension.json      (project-local)`)
}

// extList walks both the global and project-local extension dirs and
// prints a one-row-per-extension table.
func extList() error {
	type row struct {
		Scope    string
		Name     string
		Version  string
		Enabled  string
		Language string
		Dir      string
	}
	var rows []row
	for scope, dir := range extensionDirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			extDir := filepath.Join(dir, e.Name())
			mfPath := filepath.Join(extDir, "extension.json")
			raw, err := os.ReadFile(mfPath)
			if err != nil {
				continue
			}
			var m struct {
				Name     string `json:"name"`
				Version  string `json:"version"`
				Language string `json:"language"`
				Enabled  *bool  `json:"enabled"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			enabled := "yes"
			if m.Enabled != nil && !*m.Enabled {
				enabled = "no"
			}
			rows = append(rows, row{
				Scope: scope, Name: m.Name, Version: m.Version,
				Enabled: enabled, Language: m.Language, Dir: extDir,
			})
		}
	}
	if len(rows) == 0 {
		fmt.Fprintln(os.Stderr, "no extensions installed")
		fmt.Fprintln(os.Stderr, "see docs/extensions.md to write your own, or `zot ext install <path|url>`")
		return nil
	}
	fmt.Printf("%-12s  %-20s  %-10s  %-8s  %-10s  %s\n", "scope", "name", "version", "enabled", "language", "dir")
	for _, r := range rows {
		fmt.Printf("%-12s  %-20s  %-10s  %-8s  %-10s  %s\n",
			r.Scope, r.Name, dashIfEmpty(r.Version),
			r.Enabled, dashIfEmpty(r.Language), r.Dir)
	}
	return nil
}

type extDoctorHooks struct{}

func (extDoctorHooks) Notify(string, string, string)                        {}
func (extDoctorHooks) Submit(string)                                        {}
func (extDoctorHooks) SubmitSlash(string)                                   {}
func (extDoctorHooks) Insert(string)                                        {}
func (extDoctorHooks) Display(string, string)                               {}
func (extDoctorHooks) ClearNotes(string)                                    {}
func (extDoctorHooks) OpenPanel(string, extproto.PanelSpec)                 {}
func (extDoctorHooks) UpdatePanel(string, string, string, []string, string) {}
func (extDoctorHooks) ClosePanel(string, string)                            {}

type extDoctorStaticRow struct {
	Scope    string
	Name     string
	Version  string
	Enabled  bool
	Dir      string
	Exec     string
	Theme    bool
	Skills   bool
	Manifest string
	Error    string
	Shadowed bool
}

// extDoctor diagnoses extension discovery and registration without changing
// normal fail-soft extension behavior.
func extDoctor(version string) error {
	rows := scanExtDoctorStatic()
	if len(rows) == 0 {
		fmt.Fprintln(os.Stdout, "no extensions installed")
		fmt.Fprintln(os.Stdout, "see docs/extensions.md to write your own, or `zot ext install <path|url>`")
		return nil
	}

	cwd, _ := os.Getwd()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	mgr := extensions.New(ZotHome(), cwd, version, "", "", extDoctorHooks{})
	errs := mgr.Discover(ctx)
	mgr.WaitForReady(3 * time.Second)
	diags := mgr.Diagnostics()
	mgr.Stop(500 * time.Millisecond)

	diagByDir := map[string]extensions.ExtensionDiagnostic{}
	for _, d := range diags {
		diagByDir[d.Dir] = d
	}

	fmt.Fprintln(os.Stdout, "zot extension doctor")
	fmt.Fprintln(os.Stdout)
	for _, row := range rows {
		printExtDoctorRow(os.Stdout, row, diagByDir[row.Dir])
	}
	if len(errs) > 0 {
		fmt.Fprintln(os.Stdout, "load errors:")
		for _, err := range errs {
			fmt.Fprintf(os.Stdout, "  ! %v\n", err)
		}
	}
	return nil
}

func scanExtDoctorStatic() []extDoctorStaticRow {
	type scanDir struct {
		scope string
		dir   string
	}
	var dirs []scanDir
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, scanDir{scope: "project", dir: filepath.Join(cwd, ".zot", "extensions")})
	}
	if h := ZotHome(); h != "" {
		dirs = append(dirs, scanDir{scope: "global", dir: filepath.Join(h, "extensions")})
	}

	seen := map[string]bool{}
	var rows []extDoctorStaticRow
	for _, sd := range dirs {
		entries, err := os.ReadDir(sd.dir)
		if err != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			extDir := filepath.Join(sd.dir, e.Name())
			row := extDoctorStaticRow{
				Scope:    sd.scope,
				Name:     e.Name(),
				Enabled:  true,
				Dir:      extDir,
				Manifest: filepath.Join(extDir, "extension.json"),
				Theme:    extensions.HasExtensionTheme(extDir),
			}
			if seen[e.Name()] {
				row.Shadowed = true
			} else {
				seen[e.Name()] = true
			}
			raw, err := os.ReadFile(row.Manifest)
			if err != nil {
				row.Error = "missing extension.json"
				rows = append(rows, row)
				continue
			}
			var m struct {
				Name    string   `json:"name"`
				Version string   `json:"version"`
				Exec    string   `json:"exec"`
				Skills  []string `json:"skills"`
				Enabled *bool    `json:"enabled"`
			}
			if err := json.Unmarshal(raw, &m); err != nil {
				row.Error = "parse manifest: " + err.Error()
				rows = append(rows, row)
				continue
			}
			if m.Name != "" {
				row.Name = m.Name
			} else {
				row.Error = "manifest: name is required"
			}
			row.Version = m.Version
			row.Exec = m.Exec
			row.Skills = len(m.Skills) > 0
			if m.Enabled != nil {
				row.Enabled = *m.Enabled
			}
			if row.Exec == "" && !row.Theme && !row.Skills && row.Error == "" {
				row.Error = "manifest: exec, theme, or skills is required"
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func printExtDoctorRow(w io.Writer, row extDoctorStaticRow, diag extensions.ExtensionDiagnostic) {
	status := "ok"
	switch {
	case row.Error != "":
		status = "error"
	case row.Shadowed:
		status = "shadowed"
	case !row.Enabled:
		status = "disabled"
	case row.Exec == "" && row.Theme && !row.Skills:
		status = "theme-only"
	case row.Exec == "" && row.Skills && !row.Theme:
		status = "skill-only"
	case row.Exec == "" && row.Theme && row.Skills:
		status = "static-only"
	case diag.Name == "":
		status = "not loaded"
	case diag.ReadyTimedOut:
		status = "ready-timeout"
	case diag.AutoReady:
		status = "auto-ready"
	case !diag.Ready:
		status = "not ready"
	case len(diag.Messages) > 0:
		status = "warning"
	}
	fmt.Fprintf(w, "%s [%s] %s (%s)\n", row.Name, row.Scope, status, row.Dir)
	if row.Version != "" {
		fmt.Fprintf(w, "  version: %s\n", row.Version)
	}
	if row.Error != "" {
		if row.Exec != "" {
			fmt.Fprintf(w, "  log: %s\n", extDoctorLogPath(row.Name))
		}
		fmt.Fprintf(w, "  error: %s\n", row.Error)
		return
	}
	if row.Shadowed {
		fmt.Fprintln(w, "  note: skipped because a higher-priority extension directory with this name wins")
		return
	}
	if !row.Enabled {
		fmt.Fprintln(w, "  note: disabled in extension.json")
		return
	}
	if row.Exec == "" && (row.Theme || row.Skills) {
		fmt.Fprintf(w, "  type: %s\n", status)
		return
	}
	if row.Exec != "" {
		fmt.Fprintf(w, "  exec: %s\n", row.Exec)
	}
	logPath := diag.LogPath
	if logPath == "" && row.Exec != "" {
		logPath = extDoctorLogPath(row.Name)
	}
	if logPath != "" {
		fmt.Fprintf(w, "  log: %s\n", logPath)
	}
	if len(diag.Commands) > 0 {
		fmt.Fprintln(w, "  commands:")
		for _, c := range diag.Commands {
			state := "active"
			if !c.Active {
				state = "shadowed"
			}
			fmt.Fprintf(w, "    /%s (%s)\n", c.Name, state)
		}
	}
	if len(diag.Tools) > 0 {
		fmt.Fprintln(w, "  tools:")
		for _, t := range diag.Tools {
			state := "active"
			if !t.Active {
				state = "shadowed"
			}
			fmt.Fprintf(w, "    %s (%s)\n", t.Name, state)
		}
	}
	for _, msg := range diag.Messages {
		fmt.Fprintf(w, "  warning: %s\n", msg)
	}
}

func extDoctorLogPath(name string) string {
	if name == "" || ZotHome() == "" {
		return ""
	}
	return filepath.Join(ZotHome(), "logs", "ext-"+name+".log")
}

// extLogs locates the named extension's log file and either cats or
// tails it (-f).
func extLogs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: zot ext logs <name> [-f]")
	}
	name := args[0]
	follow := false
	for _, a := range args[1:] {
		if a == "-f" || a == "--follow" {
			follow = true
		}
	}
	logPath := filepath.Join(ZotHome(), "logs", "ext-"+name+".log")
	if _, err := os.Stat(logPath); err != nil {
		return fmt.Errorf("no log for %q at %s", name, logPath)
	}
	if !follow {
		f, err := os.Open(logPath)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(os.Stdout, f)
		return err
	}
	cmd := exec.Command("tail", "-F", logPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// extToggle flips the enabled flag in an extension's manifest.
func extToggle(args []string, enabled bool) error {
	if len(args) == 0 {
		verb := "enable"
		if !enabled {
			verb = "disable"
		}
		return fmt.Errorf("usage: zot ext %s <name>", verb)
	}
	name := args[0]
	dir, err := findExtensionDir(name)
	if err != nil {
		return err
	}
	mfPath := filepath.Join(dir, "extension.json")
	raw, err := os.ReadFile(mfPath)
	if err != nil {
		return err
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	generic["enabled"] = enabled
	out, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(mfPath, append(out, '\n'), 0o644); err != nil {
		return err
	}
	state := "enabled"
	if !enabled {
		state = "disabled"
	}
	fmt.Fprintf(os.Stderr, "%s %s\n", state, name)
	return nil
}

// extRemove deletes an extension's directory after a confirmation
// prompt (skip with --yes).
func extRemove(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: zot ext remove <name> [--yes]")
	}
	name := args[0]
	yes := false
	for _, a := range args[1:] {
		if a == "--yes" || a == "-y" {
			yes = true
		}
	}
	dir, err := findExtensionDir(name)
	if err != nil {
		return err
	}
	if !yes {
		fmt.Fprintf(os.Stderr, "remove %s ? [y/N] ", dir)
		var resp string
		_, _ = fmt.Scanln(&resp)
		if !strings.EqualFold(strings.TrimSpace(resp), "y") {
			fmt.Fprintln(os.Stderr, "aborted")
			return nil
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", dir)
	return nil
}

// extInstall copies a local directory or shallow-clones a git URL
// into $ZOT_HOME/extensions/. Validates the destination contains an
// extension.json before reporting success.
func extInstall(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: zot ext install <path|git-url>")
	}
	src := args[0]
	dest := filepath.Join(ZotHome(), "extensions")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	if strings.HasPrefix(src, "https://") || strings.HasPrefix(src, "git@") || strings.HasSuffix(src, ".git") {
		// Git clone path. Pick the destination name from the repo basename.
		name := strings.TrimSuffix(filepath.Base(src), ".git")
		out := filepath.Join(dest, name)
		if _, err := os.Stat(out); err == nil {
			return fmt.Errorf("destination %s already exists; remove it first", out)
		}
		cmd := exec.Command("git", "clone", "--depth", "1", src, out)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("git clone: %w", err)
		}
		if _, err := os.Stat(filepath.Join(out, "extension.json")); err != nil {
			_ = os.RemoveAll(out)
			return fmt.Errorf("installed dir lacks extension.json; aborted and rolled back")
		}
		fmt.Fprintf(os.Stderr, "installed %s\n", out)
		return nil
	}

	// Local path: must be a directory containing extension.json.
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("not a directory: %s", src)
	}
	if _, err := os.Stat(filepath.Join(src, "extension.json")); err != nil {
		return fmt.Errorf("source lacks extension.json")
	}
	// Resolve to an absolute, cleaned path before deriving the install
	// name. Otherwise relative sources like "." or "./" collapse to a
	// basename of ".", and the destination wrongly resolves to the
	// extensions/ parent directory (which zot creates on first run),
	// triggering a false "already exists" failure.
	absSrc, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	name := filepath.Base(absSrc)
	if name == "." || name == ".." || name == string(filepath.Separator) || name == "" {
		return fmt.Errorf("cannot derive extension name from %q", src)
	}
	out := filepath.Join(dest, name)
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("destination %s already exists; remove it first", out)
	}
	if err := copyDir(absSrc, out); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "installed %s\n", out)
	return nil
}

func extensionDirs() map[string]string {
	out := map[string]string{}
	if h := ZotHome(); h != "" {
		out["global"] = filepath.Join(h, "extensions")
	}
	if cwd, err := os.Getwd(); err == nil {
		out["project"] = filepath.Join(cwd, ".zot", "extensions")
	}
	return out
}

func findExtensionDir(name string) (string, error) {
	// Scan installed entries rather than joining user input onto a root.
	// Manifest names and directory names are both accepted, but neither
	// takes precedence when they identify different installations.
	matches := map[string]bool{}
	for _, dir := range extensionDirs() {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read extensions directory %s: %w", dir, err)
		}
		for _, entry := range entries {
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			candidate := filepath.Join(dir, entry.Name())
			raw, err := os.ReadFile(filepath.Join(candidate, "extension.json"))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return "", fmt.Errorf("read extension manifest in %s: %w", candidate, err)
			}
			var manifest struct {
				Name string `json:"name"`
			}
			manifestMatch := json.Unmarshal(raw, &manifest) == nil && manifest.Name != "" && manifest.Name == name
			if entry.Name() == name || manifestMatch {
				matches[candidate] = true
			}
		}
	}
	paths := make([]string, 0, len(matches))
	for path := range matches {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	switch len(paths) {
	case 0:
		return "", fmt.Errorf("extension %q not found", name)
	case 1:
		return paths[0], nil
	default:
		return "", fmt.Errorf("extension %q is ambiguous, matches: %s", name, strings.Join(paths, ", "))
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// copyDir does a recursive copy of src to dst preserving file mode
// bits. Used by `zot ext install <local-path>`.
//
// Entries matched by the source's root .gitignore are skipped, and
// .git itself is always skipped. This keeps non-portable, regeneratable
// directories (e.g. .venv with hardcoded rpaths, node_modules, target/)
// out of the installed copy so the extension stays functional at its new
// location.
func copyDir(src, dst string) error {
	ig := loadGitignore(src)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel != "." {
			name := filepath.Base(rel)
			if info.IsDir() && name == ".git" {
				return filepath.SkipDir
			}
			if ig.Match(filepath.ToSlash(rel), info.IsDir()) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// gitignore matching lives in internal/ignore so the @-file picker in
// packages/agent/modes can share it without an import cycle. These
// thin aliases keep the existing call sites (and tests) terse.
type gitignore = ignore.Gitignore

func loadGitignore(root string) *gitignore { return ignore.Load(root) }

func loadGitignoreFromString(data string) *gitignore { return ignore.Parse(data) }
