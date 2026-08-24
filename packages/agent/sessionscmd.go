package agent

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

type sessionsPruneOptions struct {
	dryRun    bool
	all       bool
	yes       bool
	olderThan *sessionAge
	cwd       string
}

type sessionAge struct {
	value int
	unit  string
}

type statPathFunc func(string) (fs.FileInfo, error)

// runSessionsCommand dispatches standalone session management commands before
// normal agent startup, so they do not require a provider or credentials.
func runSessionsCommand(rawArgs []string) (handled bool, err error) {
	if len(rawArgs) == 0 || rawArgs[0] != "sessions" {
		return false, nil
	}
	if len(rawArgs) == 1 {
		printSessionsHelp(os.Stdout)
		return true, nil
	}
	switch rawArgs[1] {
	case "help", "-h", "--help":
		printSessionsHelp(os.Stdout)
		return true, nil
	case "prune":
		opts, err := parseSessionsPruneOptions(rawArgs[2:])
		if errors.Is(err, errSessionsPruneHelp) {
			printSessionsHelp(os.Stdout)
			return true, nil
		}
		if err != nil {
			printSessionsHelp(os.Stderr)
			return true, err
		}
		return true, runSessionsPrune(opts, os.Stdin, os.Stdout, os.Stderr, os.Stat)
	default:
		printSessionsHelp(os.Stderr)
		return true, fmt.Errorf("unknown sessions subcommand: %s", rawArgs[1])
	}
}

func parseSessionsPruneOptions(args []string) (sessionsPruneOptions, error) {
	var opts sessionsPruneOptions
	for idx := 0; idx < len(args); idx++ {
		arg := args[idx]
		switch arg {
		case "--dry-run":
			opts.dryRun = true
		case "--all":
			opts.all = true
		case "-y", "--yes":
			opts.yes = true
		case "--older-than":
			if idx+1 >= len(args) {
				return opts, fmt.Errorf("--older-than requires an age")
			}
			idx++
			age, err := parseSessionAge(args[idx])
			if err != nil {
				return opts, err
			}
			opts.olderThan = &age
		case "--cwd":
			if idx+1 >= len(args) {
				return opts, fmt.Errorf("--cwd requires a path")
			}
			idx++
			if strings.TrimSpace(args[idx]) == "" {
				return opts, fmt.Errorf("--cwd requires a non-empty path")
			}
			opts.cwd = args[idx]
		case "-h", "--help":
			return opts, errSessionsPruneHelp
		default:
			return opts, fmt.Errorf("unknown sessions prune flag: %s", arg)
		}
	}
	if opts.yes && !opts.all {
		return opts, fmt.Errorf("--yes requires --all")
	}
	if opts.yes && opts.dryRun {
		return opts, fmt.Errorf("--yes cannot be used with --dry-run")
	}
	if opts.cwd != "" && opts.olderThan == nil {
		return opts, fmt.Errorf("--cwd requires --older-than")
	}
	return opts, nil
}

func parseSessionAge(value string) (sessionAge, error) {
	value = strings.TrimSpace(value)
	digitEnd := 0
	for digitEnd < len(value) && value[digitEnd] >= '0' && value[digitEnd] <= '9' {
		digitEnd++
	}
	if digitEnd == 0 || digitEnd == len(value) {
		return sessionAge{}, fmt.Errorf("invalid age %q (use m, h, d, w, mo, or y)", value)
	}
	amount, err := strconv.ParseInt(value[:digitEnd], 10, 32)
	if err != nil || amount <= 0 {
		return sessionAge{}, fmt.Errorf("invalid age %q (value must be a positive whole number)", value)
	}
	unit := value[digitEnd:]
	switch unit {
	case "m", "h", "d", "w", "mo", "y":
	default:
		return sessionAge{}, fmt.Errorf("invalid age unit %q (use m, h, d, w, mo, or y)", unit)
	}
	age := sessionAge{value: int(amount), unit: unit}
	if _, err := age.cutoff(time.Unix(0, 0)); err != nil {
		return sessionAge{}, fmt.Errorf("invalid age %q: %w", value, err)
	}
	return age, nil
}

func (age sessionAge) cutoff(now time.Time) (time.Time, error) {
	var unit time.Duration
	switch age.unit {
	case "m":
		unit = time.Minute
	case "h":
		unit = time.Hour
	case "d":
		unit = 24 * time.Hour
	case "w":
		unit = 7 * 24 * time.Hour
	case "mo":
		return subtractCalendarAge(now, 0, age.value), nil
	case "y":
		return subtractCalendarAge(now, age.value, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported unit %q", age.unit)
	}
	if int64(age.value) > int64((time.Duration(1<<63-1))/unit) {
		return time.Time{}, fmt.Errorf("duration is too large")
	}
	return now.Add(-time.Duration(age.value) * unit), nil
}

// subtractCalendarAge clamps the day to the target month's final day. This
// makes one month before March 31 the end of February rather than early March.
func subtractCalendarAge(now time.Time, years, months int) time.Time {
	first := time.Date(now.Year(), now.Month(), 1, now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), now.Location())
	target := first.AddDate(-years, -months, 0)
	lastDay := target.AddDate(0, 1, -1).Day()
	day := now.Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(target.Year(), target.Month(), day, now.Hour(), now.Minute(), now.Second(), now.Nanosecond(), now.Location())
}

var errSessionsPruneHelp = errors.New("sessions prune help requested")

func printSessionsHelp(out io.Writer) {
	fmt.Fprintln(out, `zot sessions - manage stored sessions

usage:
  zot sessions prune                         select missing directory groups
  zot sessions prune --older-than 30d        select sessions inactive for 30 days
  zot sessions prune --older-than 1mo --cwd PATH
  zot sessions prune --dry-run               list matches without deleting
  zot sessions prune --all [--yes]           select every match, optionally without prompting

Age units are m (minutes), h (hours), d (24-hour days), w (7-day weeks),
mo (calendar months), and y (calendar years). Age uses last session activity.
Without --older-than, only sessions whose recorded directory is missing match.
A missing directory cannot be distinguished from some unmounted filesystems.
Unreadable and malformed entries are preserved.`)
}

func runSessionsPrune(opts sessionsPruneOptions, in io.Reader, out, errOut io.Writer, stat statPathFunc) error {
	groups, scanIssues := core.ScanStoredSessionGroups(SessionsPath())
	for _, issue := range scanIssues {
		fmt.Fprintf(errOut, "warning: preserving %s: %v\n", issue.Path, issue.Err)
	}

	candidates := make([]core.StoredSessionGroup, 0, len(groups))
	var cutoff time.Time
	if opts.olderThan != nil {
		var err error
		cutoff, err = opts.olderThan.cutoff(time.Now())
		if err != nil {
			return err
		}
		cwd := opts.cwd
		if cwd != "" {
			cwd, err = filepath.Abs(cwd)
			if err != nil {
				return fmt.Errorf("resolve --cwd: %w", err)
			}
			cwd = filepath.Clean(cwd)
		}
		for _, group := range groups {
			if cwd != "" && filepath.Clean(group.CWD) != cwd {
				continue
			}
			matching := core.StoredSessionGroup{CWD: group.CWD}
			for _, path := range group.Paths {
				info, err := stat(path)
				if err != nil {
					fmt.Fprintf(errOut, "warning: preserving %s: %v\n", path, err)
					continue
				}
				if info.ModTime().Before(cutoff) {
					matching.Paths = append(matching.Paths, path)
					matching.SizeBytes += info.Size()
				}
			}
			if len(matching.Paths) > 0 {
				candidates = append(candidates, matching)
			}
		}
	} else {
		for _, group := range groups {
			if !filepath.IsAbs(group.CWD) {
				fmt.Fprintf(errOut, "warning: preserving sessions with non-absolute cwd %q\n", group.CWD)
				continue
			}
			_, err := stat(group.CWD)
			switch {
			case err == nil:
				continue
			case errors.Is(err, fs.ErrNotExist):
				candidates = append(candidates, group)
			default:
				fmt.Fprintf(errOut, "warning: preserving sessions for %s: %v\n", group.CWD, err)
			}
		}
	}
	if len(candidates) == 0 {
		if opts.olderThan != nil {
			fmt.Fprintln(out, "no sessions older than the requested age found")
		} else {
			fmt.Fprintln(out, "no stale session directories found")
		}
		return nil
	}

	if opts.olderThan != nil {
		fmt.Fprintln(out, "session directories with inactive sessions:")
	} else {
		fmt.Fprintln(out, "missing session directories:")
	}
	for idx, group := range candidates {
		fmt.Fprintf(out, "  %d. %s (%s, %s)\n", idx+1, group.CWD, sessionCount(len(group.Paths)), provider.FormatBytes(group.SizeBytes))
	}
	if opts.dryRun {
		fmt.Fprintf(out, "dry run: %s in %s would be deleted\n", sessionCount(totalSessions(candidates)), directoryCount(len(candidates)))
		return nil
	}

	reader := bufio.NewReader(in)
	selected := make([]int, 0, len(candidates))
	if opts.all {
		for idx := range candidates {
			selected = append(selected, idx)
		}
	} else {
		fmt.Fprint(out, "select groups to delete (for example 1,3-5; all; none): ")
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read selection: %w", err)
		}
		selected, err = parseSessionSelection(line, len(candidates))
		if err != nil {
			return err
		}
		if len(selected) == 0 {
			fmt.Fprintln(out, "no sessions deleted")
			return nil
		}
	}

	selectedSessions := 0
	for _, idx := range selected {
		selectedSessions += len(candidates[idx].Paths)
	}
	if !opts.yes {
		fmt.Fprintf(out, "permanently delete %s in %s? [y/N]: ", sessionCount(selectedSessions), directoryCount(len(selected)))
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read confirmation: %w", err)
		}
		answer := strings.ToLower(strings.TrimSpace(line))
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(out, "no sessions deleted")
			return nil
		}
	}

	deletedSessions := 0
	deletedGroups := 0
	var deleteErrors []error
	for _, idx := range selected {
		group := candidates[idx]
		if opts.olderThan == nil {
			_, err := stat(group.CWD)
			if err == nil {
				fmt.Fprintf(errOut, "warning: preserving sessions for %s because the directory now exists\n", group.CWD)
				continue
			}
			if !errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(errOut, "warning: preserving sessions for %s after recheck: %v\n", group.CWD, err)
				continue
			}
		}

		groupDeleted := 0
		for _, path := range group.Paths {
			if opts.olderThan != nil {
				info, err := stat(path)
				if err != nil {
					fmt.Fprintf(errOut, "warning: preserving %s after recheck: %v\n", path, err)
					continue
				}
				if !info.ModTime().Before(cutoff) {
					fmt.Fprintf(errOut, "warning: preserving %s because it now has recent activity\n", path)
					continue
				}
			}
			if err := core.DeleteSession(path); err != nil {
				deleteErrors = append(deleteErrors, fmt.Errorf("delete %s: %w", path, err))
				continue
			}
			groupDeleted++
			_ = os.Remove(filepath.Dir(path))
		}
		deletedSessions += groupDeleted
		if groupDeleted == len(group.Paths) {
			deletedGroups++
		}
	}
	fmt.Fprintf(out, "deleted %s from %s\n", sessionCount(deletedSessions), directoryCount(deletedGroups))
	return errors.Join(deleteErrors...)
}

func parseSessionSelection(input string, count int) ([]int, error) {
	input = strings.ToLower(strings.TrimSpace(input))
	switch input {
	case "", "none", "n":
		return nil, nil
	case "all", "a":
		selected := make([]int, count)
		for idx := range selected {
			selected[idx] = idx
		}
		return selected, nil
	}

	seen := make(map[int]bool)
	for _, field := range strings.Split(input, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			return nil, fmt.Errorf("invalid empty selection")
		}
		startText, endText, isRange := strings.Cut(field, "-")
		start, err := strconv.Atoi(strings.TrimSpace(startText))
		if err != nil {
			return nil, fmt.Errorf("invalid selection %q", field)
		}
		end := start
		if isRange {
			end, err = strconv.Atoi(strings.TrimSpace(endText))
			if err != nil || end < start {
				return nil, fmt.Errorf("invalid selection range %q", field)
			}
		}
		if start < 1 || end > count {
			return nil, fmt.Errorf("selection %q is outside 1-%d", field, count)
		}
		for value := start; value <= end; value++ {
			seen[value-1] = true
		}
	}
	selected := make([]int, 0, len(seen))
	for idx := range seen {
		selected = append(selected, idx)
	}
	sort.Ints(selected)
	return selected, nil
}

func totalSessions(groups []core.StoredSessionGroup) int {
	total := 0
	for _, group := range groups {
		total += len(group.Paths)
	}
	return total
}

func sessionCount(count int) string {
	if count == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", count)
}

func directoryCount(count int) string {
	if count == 1 {
		return "1 directory"
	}
	return fmt.Sprintf("%d directories", count)
}
