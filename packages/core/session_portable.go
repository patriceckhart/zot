package core

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/patriceckhart/zot/packages/provider"
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
			p, err := firstUserPrompt(src)
			if err != nil {
				return "", fmt.Errorf("export: read first prompt: %w", err)
			}
			firstPrompt = p
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

	// Stream every non-meta row verbatim. Use ReadBytes instead of
	// bufio.Scanner: large sessions can contain very long JSONL rows
	// (image blocks, big tool outputs, compacted history) that exceed
	// Scanner's token limit and fail with "token too long".
	r := bufio.NewReader(src)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			var h sessionLineHead
			if err := json.Unmarshal(line, &h); err == nil && h.Type != "meta" {
				if _, werr := bw.Write(line); werr != nil {
					return "", werr
				}
				if werr := bw.WriteByte('\n'); werr != nil {
					return "", werr
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("export: read source: %w", err)
		}
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

	// Imported aliases are local identities, never copied from the source.
	dst, lock, alias, err := createAliasedFile(root, cwd, "")
	if err != nil {
		return "", fmt.Errorf("import: create dst: %w", err)
	}
	defer lock.Close()
	defer dst.Close()
	outPath := dst.Name()
	newID := uuid.NewString()
	bw := bufio.NewWriter(dst)

	// Write a fresh meta row claiming ownership.
	importMeta := SessionMeta{
		ID:       newID,
		Alias:    alias,
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

	// Rewind the source and stream every non-meta row. Avoid
	// bufio.Scanner so exported sessions with huge JSONL rows import
	// cleanly.
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("import: rewind: %w", err)
	}
	if err := forEachJSONLLine(src, func(line []byte) error {
		var h sessionLineHead
		if err := json.Unmarshal(line, &h); err != nil || h.Type == "meta" {
			return nil
		}
		if _, err := bw.Write(line); err != nil {
			return err
		}
		return bw.WriteByte('\n')
	}); err != nil {
		return "", fmt.Errorf("import: read source: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	return outPath, nil
}

// BranchSession creates a new session in root/cwd that contains the
// parent's messages 0..upToMessageIdx-1 (i.e. the first N user+
// assistant+tool rows). The new meta records Parent=<parent id> and
// ForkPoint=N so /session tree can rebuild the branch topology
// later. All non-message rows (usage) are preserved up to the cut
// point so the running cost tracker stays accurate.
//
// upToMessageIdx is a count over the flat message stream as
// returned by OpenSession. To "branch at user turn 3" the caller
// passes the index of that user message in msgs + 1 (so the
// message itself is included). The caller figures that out; this
// helper just copies the first N message rows.
//
// Returns the path of the new session file, ready for OpenSession.
func BranchSession(parentPath, root, cwd, version string, upToMessageIdx int) (string, error) {
	return branchSession(parentPath, root, cwd, version, upToMessageIdx, false)
}

// BranchSessionHidden creates a branch that participates in /session tree but
// is hidden from the flat /sessions picker. Used for in-place tree navigation.
func BranchSessionHidden(parentPath, root, cwd, version string, upToMessageIdx int) (string, error) {
	return branchSession(parentPath, root, cwd, version, upToMessageIdx, true)
}

func branchSession(parentPath, root, cwd, version string, upToMessageIdx int, hideFromSessions bool) (string, error) {
	if parentPath == "" {
		return "", errors.New("branch: parent path is empty")
	}
	if upToMessageIdx < 0 {
		return "", errors.New("branch: upToMessageIdx must be >= 0")
	}

	src, err := os.Open(parentPath)
	if err != nil {
		return "", fmt.Errorf("branch: open parent: %w", err)
	}
	defer src.Close()

	// Read the parent meta so we can copy model/provider and record
	// the parent id on the child.
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	if !sc.Scan() {
		return "", errors.New("branch: parent session is empty")
	}
	var head sessionLine
	if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
		return "", fmt.Errorf("branch: parse parent meta: %w", err)
	}
	if head.Type != "meta" || head.Meta == nil {
		return "", errors.New("branch: parent first line is not a meta row")
	}
	parentMeta := *head.Meta

	// Branches receive their own alias without changing parent UUID links.
	dst, lock, alias, err := createAliasedFile(root, cwd, "")
	if err != nil {
		return "", fmt.Errorf("branch: create dst: %w", err)
	}
	defer lock.Close()
	defer dst.Close()
	outPath := dst.Name()
	newID := uuid.NewString()
	bw := bufio.NewWriter(dst)

	// Write the branch meta.
	branchMeta := SessionMeta{
		ID:               newID,
		Alias:            alias,
		CWD:              cwd,
		Model:            parentMeta.Model,
		Provider:         parentMeta.Provider,
		Started:          time.Now().UTC(),
		Version:          version,
		Parent:           parentMeta.ID,
		ForkPoint:        upToMessageIdx,
		HideFromSessions: hideFromSessions,
	}
	metaLine, err := json.Marshal(sessionLine{Type: "meta", Meta: &branchMeta})
	if err != nil {
		return "", fmt.Errorf("branch: marshal meta: %w", err)
	}
	if _, err := bw.Write(metaLine); err != nil {
		return "", err
	}
	if err := bw.WriteByte('\n'); err != nil {
		return "", err
	}

	// Reconstruct the effective transcript the same way OpenSession
	// does: message rows append, and compaction rows replace everything
	// before them. The fork index is defined over that effective stream,
	// not over the raw audit rows kept on disk before a compaction.
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("branch: rewind parent: %w", err)
	}
	var effective []provider.Message
	var nonCompactedRows [][]byte
	effectiveCount := 0
	sawCompaction := false
	if err := forEachJSONLLine(src, func(line []byte) error {
		var h sessionLineHead
		if err := json.Unmarshal(line, &h); err != nil {
			return nil
		}
		switch h.Type {
		case "message":
			if msg, err := hydrateMessage(line); err == nil && len(msg.Content) > 0 {
				effective = append(effective, msg)
				if !sawCompaction && effectiveCount < upToMessageIdx {
					raw := append([]byte(nil), line...)
					nonCompactedRows = append(nonCompactedRows, raw)
				}
				effectiveCount++
			}
		case "compaction":
			if compacted, err := hydrateCompaction(line); err == nil {
				effective = compacted
				effectiveCount = len(effective)
				sawCompaction = true
			}
		case "usage":
			if !sawCompaction && effectiveCount < upToMessageIdx {
				raw := append([]byte(nil), line...)
				nonCompactedRows = append(nonCompactedRows, raw)
			}
		}
		return nil
	}); err != nil && err != io.EOF {
		return "", fmt.Errorf("branch: read parent: %w", err)
	}
	limit := upToMessageIdx
	if limit > len(effective) {
		limit = len(effective)
	}
	branchTitle := lastUserText(effective[:limit])
	if sawCompaction {
		for i := 0; i < limit; i++ {
			msg := effective[i]
			line, err := json.Marshal(sessionLine{Type: "message", Message: &msg})
			if err != nil {
				return "", fmt.Errorf("branch: marshal message: %w", err)
			}
			if _, err := bw.Write(line); err != nil {
				return "", err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return "", err
			}
		}
	} else {
		for _, row := range nonCompactedRows {
			if _, err := bw.Write(row); err != nil {
				return "", err
			}
			if err := bw.WriteByte('\n'); err != nil {
				return "", err
			}
		}
	}
	if branchTitle != "" {
		line, _ := json.Marshal(map[string]string{"type": "rename", "title": branchTitle})
		if _, err := bw.Write(line); err != nil {
			return "", err
		}
		if err := bw.WriteByte('\n'); err != nil {
			return "", err
		}
	}
	if err := bw.Flush(); err != nil {
		return "", err
	}
	return outPath, nil
}

func lastUserText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != provider.RoleUser {
			continue
		}
		if text := strings.TrimSpace(firstTextFromMessage(msgs[i])); text != "" {
			return text
		}
	}
	return ""
}

// TreeNode is one entry in the branch tree returned by
// BuildSessionTree. Children are populated by linking on Parent ID.
type TreeNode struct {
	Summary  SessionSummary
	Meta     SessionMeta
	Children []*TreeNode
}

// BuildSessionTree loads every session in the cwd dir and returns
// the forest rooted at parentless sessions, with each non-root
// session placed under its parent. Used by /session tree to render
// the branch hierarchy.
func BuildSessionTree(root, cwd string) []*TreeNode {
	dir := SessionsDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	nodes := make(map[string]*TreeNode)
	order := []string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		summary := describeSession(path)
		meta, _ := readSessionMeta(path)
		if meta.ID == "" {
			continue
		}
		nodes[meta.ID] = &TreeNode{Summary: summary, Meta: meta}
		order = append(order, meta.ID)
	}
	var roots []*TreeNode
	for _, id := range order {
		n := nodes[id]
		if n.Meta.Parent == "" {
			roots = append(roots, n)
			continue
		}
		if parent, ok := nodes[n.Meta.Parent]; ok {
			parent.Children = append(parent.Children, n)
		} else {
			// Parent file missing (was manually deleted). Treat as
			// a root so it still shows up in the tree.
			roots = append(roots, n)
		}
	}
	return roots
}

// readSessionMeta opens path, reads the meta row, and returns it.
// Empty SessionMeta when the file is missing or not a valid session.
func readSessionMeta(path string) (SessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionMeta{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 20*1024*1024)
	if !sc.Scan() {
		return SessionMeta{}, errors.New("empty file")
	}
	var line sessionLine
	if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
		return SessionMeta{}, err
	}
	if line.Type != "meta" || line.Meta == nil {
		return SessionMeta{}, errors.New("first line is not meta")
	}
	return *line.Meta, nil
}

// FindSessionByID looks up a session file in root/cwd whose meta id
// matches. Used by /session tree when the user picks an entry. O(n)
// over the files in the dir; the list is small in practice.
func FindSessionByID(root, cwd, id string) string {
	dir := SessionsDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		meta, err := readSessionMeta(path)
		if err != nil {
			continue
		}
		if meta.ID == id {
			return path
		}
	}
	return ""
}

// firstUserPrompt scans forward from the current source position
// looking for the first user-role message and returns its text.
// Used to build a humane export filename. Uses Reader instead of
// Scanner so a very large JSONL row before the first user prompt
// cannot trip Scanner's token limit.
func firstUserPrompt(src io.Reader) (string, error) {
	r := bufio.NewReader(src)
	for {
		lineBytes, err := r.ReadBytes('\n')
		if len(lineBytes) > 0 {
			lineBytes = bytes.TrimRight(lineBytes, "\r\n")
			var line sessionLine
			if err := json.Unmarshal(lineBytes, &line); err == nil {
				if line.Type == "message" && line.Message != nil && line.Message.Role == "user" {
					b, _ := json.Marshal(line.Message)
					var m struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					}
					_ = json.Unmarshal(b, &m)
					for _, c := range m.Content {
						if c.Text != "" {
							return c.Text, nil
						}
					}
				}
			}
		}
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
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
