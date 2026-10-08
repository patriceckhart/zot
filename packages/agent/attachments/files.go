// Package attachments prepares explicitly selected client files for transfer.
package attachments

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/patriceckhart/zot/packages/agent/tools"
	"github.com/patriceckhart/zot/packages/continuous"
)

// ReadFiles reads regular files on the client with bounded memory. Paths are
// resolved here, the host receives names and bytes and never opens these paths.
func ReadFiles(ctx context.Context, cwd string, paths []string, sandbox *tools.Sandbox) ([]continuous.FileAttachment, error) {
	if len(paths) > continuous.MaxAttachments {
		return nil, fmt.Errorf("at most %d attachments", continuous.MaxAttachments)
	}
	var files []continuous.FileAttachment
	size := 0
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			path = filepath.Join(home, path[2:])
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if err := sandbox.CheckPath(path); err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("attach file %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("attachment %s must be a regular file", path)
		}
		remaining := continuous.MaxAttachmentBytes - size
		if info.Size() > int64(remaining) {
			return nil, fmt.Errorf("attachments exceed %d bytes", continuous.MaxAttachmentBytes)
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("attach file %s: %w", path, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(f, int64(remaining)+1))
		closeErr := f.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(data) > remaining {
			return nil, fmt.Errorf("attachments exceed %d bytes", continuous.MaxAttachmentBytes)
		}
		size += len(data)
		files = append(files, continuous.FileAttachment{Name: filepath.Base(path), Data: data})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return files, nil
}
