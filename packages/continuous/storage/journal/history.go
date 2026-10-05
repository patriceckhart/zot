package journal

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/internal/state"
)

var errPageComplete = errors.New("journal scan page complete")

// scanPage reads only committed authority using positional I/O, never changing
// the append cursor. It retains at most one scan page, with no historical cache
// or in-memory offset index. Until a rebuildable disk index exists, every page
// scans and validates the prefix from revision one through the requested page.
// The caller serializes this with persistence and close.
func scanPage(ctx context.Context, data io.ReaderAt, end int64, revision, after uint64, limit int) ([]storage.Commit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if after > revision {
		return nil, storage.ErrCursor
	}
	if limit < 1 || limit > state.MaxPage {
		return nil, fmt.Errorf("scan limit must be between 1 and %d", state.MaxPage)
	}
	if after == revision {
		return nil, nil
	}
	out := make([]storage.Commit, 0, min(uint64(limit), revision-after))
	_, err := scanFrames(ctx, io.NewSectionReader(data, 0, end), end, boundaryMarker(revision, end), func(c storage.Commit) error {
		if c.Revision > after {
			out = append(out, c)
			if len(out) == limit && c.Revision < revision {
				return errPageComplete
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errPageComplete) {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
