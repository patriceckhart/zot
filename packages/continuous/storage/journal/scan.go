package journal

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/internal/state"
)

// scanJournal streams the committed prefix through a shared validator. Inspection
// does not retain history or construct the current record index.
func scanJournal(ctx context.Context, data *journalData, marker []byte, visit func(storage.Commit) error) (Verification, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	size := data.Size()
	return scanFrames(ctx, io.NewSectionReader(data, 0, size), size, marker, visit)
}

func scanFrames(ctx context.Context, data io.Reader, size int64, marker []byte, visit func(storage.Commit) error) (Verification, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	if len(marker) != 20 || crc32.Checksum(marker[:16], table) != binary.BigEndian.Uint32(marker[16:20]) {
		return Verification{}, storage.ErrCorrupt
	}
	revision := binary.BigEndian.Uint64(marker[:8])
	rawEnd := binary.BigEndian.Uint64(marker[8:16])
	if rawEnd > uint64(^uint64(0)>>1) {
		return Verification{}, storage.ErrCorrupt
	}
	end := int64(rawEnd)
	// Every commit needs a header and at least one payload byte.
	if size < end || revision > uint64(end)/(headerSize+1) {
		return Verification{}, storage.ErrCorrupt
	}
	var pos int64
	var epoch uint64
	for r := uint64(1); r <= revision; r++ {
		if err := ctx.Err(); err != nil {
			return Verification{}, err
		}
		var h [headerSize]byte
		if _, err := io.ReadFull(data, h[:]); err != nil {
			return Verification{}, storage.ErrCorrupt
		}
		length := binary.BigEndian.Uint32(h[4:8])
		if string(h[:4]) != string(magic[:]) || length == 0 || length > state.MaxCommitBytes || binary.BigEndian.Uint64(h[8:16]) != r || crc32.Checksum(h[:20], table) != binary.BigEndian.Uint32(h[20:24]) {
			return Verification{}, storage.ErrCorrupt
		}
		pos += headerSize + int64(length)
		if pos > end {
			return Verification{}, storage.ErrCorrupt
		}
		b := make([]byte, length)
		if _, err := io.ReadFull(data, b); err != nil {
			return Verification{}, storage.ErrCorrupt
		}
		if crc32.Checksum(b, table) != binary.BigEndian.Uint32(h[16:20]) {
			return Verification{}, storage.ErrCorrupt
		}
		var c storage.Commit
		if err := json.Unmarshal(b, &c); err != nil {
			return Verification{}, storage.ErrCorrupt
		}
		if err := state.ValidateCommit(c, r, epoch); err != nil {
			return Verification{}, err
		}
		epoch = c.Epoch
		if err := ctx.Err(); err != nil {
			return Verification{}, err
		}
		if visit != nil {
			if err := visit(c); err != nil {
				return Verification{}, err
			}
		}
	}
	if pos != end {
		return Verification{}, storage.ErrCorrupt
	}
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	return Verification{
		Valid: true, Scope: "journal", Schema: 1, Revision: revision, Epoch: epoch,
		CommittedBytes: end, UnacknowledgedTailBytes: size - end,
	}, nil
}
