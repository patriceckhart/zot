package core

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// PortableExt is the filesystem extension used for exported sessions.
// A ".zotsession" is just a zot JSONL session file with the meta
// header rewritten so the importing user gets fresh ownership.
const PortableExt = ".zotsession"

// ExportSession writes the session at srcPath to dstPath as a
// portable .zotsession file. If dstPath is an existing directory the
// file is created inside it with a name derived from the session's
// meta ("YYYYMMDD-HHMMSS-<first-prompt-excerpt>.zotsession"). The
// destination's directory is created if needed. Returns the final
// resolved path so the caller can tell the user where it landed.
//
// The on-disk format is unchanged from a live session; only the
// meta.cwd is stripped of its per-machine prefix (the importing
// user doesn't care what directory it came from). Everything else
// round-trips byte-for-byte.
func ExportSession(srcPath, dstPath string) (string, error) {
	if srcPath == "" {
		return "", errors.New("export: source path is empty")
	}
	if dstPath == "" {
		return "", errors.New("export: destination path is empty")
	}

	// Read the source meta up-front so we can name the output sensibly
	// when dstPath is a directory, and so we can validate it's a real
	// session before starting to write.
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("export: open source: %w", err)
	}
	defer src.Close()

	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	if !sc.Scan() {
		return "", errors.New("export: session file is empty")
	}
	var head sessionLine
	if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
		return "", fmt.Errorf("export: parse meta: %w", err)
	}
	if head.Type != "meta" || head.Meta == nil {
		return "", errors.New("export: first line is not a meta row")
	}

	// Scan the rest of the file for the first user message so we can
	// build a humane filename. Only reads if dstPath doesn't already
	// end in .zotsession.
	firstPrompt := ""
	if !strings.HasSuffix(strings.ToLower(dstPath), PortableExt) {
		if fi, _ := os.Stat(dstPath); fi == nil || fi.IsDir() {
			firstPrompt = firstUserPrompt(sc)
		}
	}

	// Resolve dstPath: if it's a directory, build a name inside it.
	outPath := dstPath
	if fi, err := os.Stat(dstPath); err == nil && fi.IsDir() {
		name := filenameFor(head.Meta.Started, head.Meta.ID, firstPrompt)
		outPath = filepath.Join(dstPath, name)
	} else if !strings.HasSuffix(strings.ToLower(outPath), PortableExt) {
		outPath += PortableExt
	}

	// Re-open the source from the top since we advanced the scanner.
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("export: rewind: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return "", fmt.Errorf("export: mkdir dst: %w", err)
	}
	dst, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("export: create dst: %w", err)
	}
	defer dst.Close()
	bw := bufio.NewWriter(dst)

	// Rewrite the meta row: strip the cwd (the importing user has
	// their own) and keep everything else identical. ID stays so the
	// export is traceable; the importer will rotate to a fresh ID.
	exportMeta := *head.Meta
	exportMeta.CWD = ""
	metaLine, err := json.Marshal(sessionLine{Type: "meta", Meta: &exportMeta})
	if err != nil {
		return "", fmt.Errorf("export: marshal meta: %w", err)
	}
	if _, err := bw.Write(metaLine); err != nil {
		return "", err
	}
	if err := bw.WriteByte('\n'); err != nil {
		return "", err
	}

	// Stream every non-meta row verbatim.
	sc2 := bufio.NewScanner(src)
	sc2.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	for sc2.Scan() {
		line := sc2.Bytes()
		var h sessionLineHead
		if err := json.Unmarshal(line, &h); err != nil {
			continue
		}
		if h.Type == "meta" {
			continue // already wrote a rewritten copy above
		}
		if _, err := bw.Write(line); err != nil {
			return "", err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return "", err
		}
	}
	if err := sc2.Err(); err != nil {
		return "", fmt.Errorf("export: read source: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	return outPath, nil
}

// ImportSession copies the .zotsession file at srcPath into the
// running user's session store under the given root+cwd, rewriting
// the meta's id / cwd / started fields so the imported session is
// owned by the current user / directory / clock. Returns the path
// of the created session file, ready to pass to OpenSession.
//
// The imported session is a first-class zot session: it'll show up
// in /sessions, /jump, and on-disk summaries just like any other.
// Messages and usage rows are preserved verbatim.
func ImportSession(srcPath, root, cwd, version string) (string, error) {
	if srcPath == "" {
		return "", errors.New("import: source path is empty")
	}
	src, err := os.Open(srcPath)
	if err != nil {
		return "", fmt.Errorf("import: open source: %w", err)
	}
	defer src.Close()

	// Validate the file header before committing to a destination.
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	if !sc.Scan() {
		return "", errors.New("import: session file is empty")
	}
	var head sessionLine
	if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
		return "", fmt.Errorf("import: parse meta: %w", err)
	}
	if head.Type != "meta" || head.Meta == nil {
		return "", errors.New("import: first line is not a meta row")
	}

	// Build the destination inside the current cwd's session dir
	// with a fresh timestamped name.
	dir := SessionsDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	newID := uuid.NewString()
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().UTC().Format("20060102-150405"), newID[:8])
	outPath := filepath.Join(dir, name)
	dst, err := os.OpenFile(outPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("import: create dst: %w", err)
	}
	defer dst.Close()
	bw := bufio.NewWriter(dst)

	// Write a fresh meta row claiming ownership.
	importMeta := SessionMeta{
		ID:       newID,
		CWD:      cwd,
		Model:    head.Meta.Model,
		Provider: head.Meta.Provider,
		Started:  time.Now().UTC(),
		Version:  version,
	}
	metaLine, err := json.Marshal(sessionLine{Type: "meta", Meta: &importMeta})
	if err != nil {
		return "", fmt.Errorf("import: marshal meta: %w", err)
	}
	if _, err := bw.Write(metaLine); err != nil {
		return "", err
	}
	if err := bw.WriteByte('\n'); err != nil {
		return "", err
	}

	// Rewind the source and stream every non-meta row.
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("import: rewind: %w", err)
	}
	sc2 := bufio.NewScanner(src)
	sc2.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	for sc2.Scan() {
		line := sc2.Bytes()
		var h sessionLineHead
		if err := json.Unmarshal(line, &h); err != nil {
			continue
		}
		if h.Type == "meta" {
			continue
		}
		if _, err := bw.Write(line); err != nil {
			return "", err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return "", err
		}
	}
	if err := sc2.Err(); err != nil {
		return "", fmt.Errorf("import: read source: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	return outPath, nil
}

// firstUserPrompt scans forward from the current scanner position
// looking for the first user-role message and returns its text
// (trimmed, short). Used to build a humane export filename.
func firstUserPrompt(sc *bufio.Scanner) string {
	for sc.Scan() {
		var line sessionLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "message" || line.Message == nil || line.Message.Role != "user" {
			continue
		}
		for _, c := range line.Message.Content {
			// Use type name to avoid an import of provider here beyond
			// the already-imported alias; TextBlock is the only content
			// shape that yields a useful preview, so we just look for
			// something with a reasonable string form.
			s := fmt.Sprintf("%v", c)
			_ = s // formatted value is too noisy; go straight to typed path
		}
		// Simpler: marshal the message and fish out the first "text"
		// we can find. Avoids reaching into the provider package just
		// for an interface type check.
		b, _ := json.Marshal(line.Message)
		var m struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(b, &m)
		for _, c := range m.Content {
			if c.Text != "" {
				return c.Text
			}
		}
	}
	return ""
}

// filenameFor builds a descriptive .zotsession filename from the
// session's start time and, when available, an excerpt of the
// first user prompt.
func filenameFor(started time.Time, id, firstPrompt string) string {
	base := started.UTC().Format("20060102-150405")
	if id != "" && len(id) >= 8 {
		base += "-" + id[:8]
	}
	slug := slugify(firstPrompt, 40)
	if slug != "" {
		base += "-" + slug
	}
	return base + PortableExt
}

// slugify lowercases, strips punctuation, collapses whitespace to
// hyphens, and truncates to max runes so it's safe as a filename.
func slugify(s string, max int) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	var out strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && out.Len() > 0 {
				out.WriteByte('-')
				prevDash = true
			}
		}
		if out.Len() >= max {
			break
		}
	}
	return strings.TrimRight(out.String(), "-")
}
