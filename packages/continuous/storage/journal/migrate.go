package journal

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/internal/state"
)

// CurrentSchema is the commit schema written by this build. Older schemas
// listed in migrations are readable through Migrate only; Open and Verify
// accept exactly the current schema.
const CurrentSchema = 1

// Format describes a journal directory without opening it as a writer.
type Format struct {
	// Schema is the commit schema of the journal, detected from the first
	// committed frame. An empty journal reports CurrentSchema.
	Schema int `json:"schema"`
	// Current reports whether Open can use the journal as is.
	Current bool `json:"current"`
	// Migratable reports whether Migrate can rewrite it to CurrentSchema.
	Migratable bool   `json:"migratable"`
	Revision   uint64 `json:"revision"`
	Epoch      uint64 `json:"epoch"`
}

// migrations maps a source schema to the function that converts one of its
// commits into the current schema. The current schema maps to a pass-through,
// so a migration of a current store is a verified rewrite.
var migrations = map[int]func(storage.Commit) (storage.Commit, error){
	CurrentSchema: func(c storage.Commit) (storage.Commit, error) { return c, nil },
}

// ErrSchemaUnsupported reports a journal whose schema this build can neither
// open nor migrate. A newer build wrote it, or it is not a journal.
var ErrSchemaUnsupported = errors.New("journal schema unsupported by this build")

// InspectFormat reports the schema of an existing journal. It locks the
// journal exclusively for the scan, like Verify, and changes nothing. A
// journal whose frames are readable but whose commit schema is not current
// is reported with Current false rather than as corrupt.
func InspectFormat(ctx context.Context, path string) (Format, error) {
	var format Format
	_, err := readJournal(ctx, path, func(data *journalData, marker []byte) (Verification, error) {
		size := data.Size()
		f, err := scanSchema(ctx, io.NewSectionReader(data, 0, size), size, marker)
		format = f
		return Verification{}, err
	})
	return format, err
}

// scanSchema walks frames checking framing and checksums only, decoding each
// commit's schema and epoch. It does not apply schema-specific validation.
func scanSchema(ctx context.Context, data io.Reader, size int64, marker []byte) (Format, error) {
	if err := ctx.Err(); err != nil {
		return Format{}, err
	}
	if len(marker) != 20 || crc32.Checksum(marker[:16], table) != binary.BigEndian.Uint32(marker[16:20]) {
		return Format{}, storage.ErrCorrupt
	}
	revision := binary.BigEndian.Uint64(marker[:8])
	rawEnd := binary.BigEndian.Uint64(marker[8:16])
	if rawEnd > uint64(^uint64(0)>>1) {
		return Format{}, storage.ErrCorrupt
	}
	end := int64(rawEnd)
	if size < end || revision > uint64(end)/(headerSize+1) {
		return Format{}, storage.ErrCorrupt
	}
	format := Format{Revision: revision}
	var pos int64
	for r := uint64(1); r <= revision; r++ {
		if err := ctx.Err(); err != nil {
			return Format{}, err
		}
		var h [headerSize]byte
		if _, err := io.ReadFull(data, h[:]); err != nil {
			return Format{}, storage.ErrCorrupt
		}
		length := binary.BigEndian.Uint32(h[4:8])
		if string(h[:4]) != string(magic[:]) || length == 0 || length > state.MaxCommitBytes || binary.BigEndian.Uint64(h[8:16]) != r || crc32.Checksum(h[:20], table) != binary.BigEndian.Uint32(h[20:24]) {
			return Format{}, storage.ErrCorrupt
		}
		pos += headerSize + int64(length)
		if pos > end {
			return Format{}, storage.ErrCorrupt
		}
		b := make([]byte, length)
		if _, err := io.ReadFull(data, b); err != nil {
			return Format{}, storage.ErrCorrupt
		}
		if crc32.Checksum(b, table) != binary.BigEndian.Uint32(h[16:20]) {
			return Format{}, storage.ErrCorrupt
		}
		var c struct {
			Schema int    `json:"schema"`
			Epoch  uint64 `json:"epoch"`
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return Format{}, storage.ErrCorrupt
		}
		if r == 1 {
			format.Schema = c.Schema
		} else if c.Schema != format.Schema {
			return Format{}, fmt.Errorf("%w: mixed commit schemas", storage.ErrCorrupt)
		}
		format.Epoch = c.Epoch
	}
	if pos != end {
		return Format{}, storage.ErrCorrupt
	}
	if revision == 0 {
		// An empty journal has no committed schema; it opens as current.
		format.Schema = CurrentSchema
	}
	format.Current = format.Schema == CurrentSchema
	_, format.Migratable = migrations[format.Schema]
	return format, nil
}

// Migrate rewrites the committed prefix of an existing journal into a new
// journal directory at CurrentSchema. The source is verified at its own schema
// and exclusively locked through the copy, but unchanged; the destination
// must not exist and its parent must. Every commit is converted, validated
// with the current rules, and written with fresh framing. The returned
// verification describes the destination. Interrupted migrations leave a
// partial destination that fails verification; remove it and retry.
func Migrate(ctx context.Context, source, destination string, opts Options) (Verification, error) {
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	mode, err := archiveDurability(opts)
	if err != nil {
		return Verification{}, err
	}
	if err := outsideStore(source, destination); err != nil {
		return Verification{}, err
	}
	var result Verification
	_, err = readJournal(ctx, source, func(data *journalData, marker []byte) (Verification, error) {
		size := data.Size()
		format, err := scanSchema(ctx, io.NewSectionReader(data, 0, size), size, marker)
		if err != nil {
			return Verification{}, err
		}
		convert, ok := migrations[format.Schema]
		if !ok {
			return Verification{}, fmt.Errorf("%w: schema %d", ErrSchemaUnsupported, format.Schema)
		}
		result, err = writeMigrated(ctx, io.NewSectionReader(data, 0, size), format, convert, destination, mode)
		return result, err
	})
	return result, err
}

// writeMigrated creates the destination journal and streams converted commits
// into it. The boundary is published only after the complete data file is
// written and synchronized, exactly as Restore does.
func writeMigrated(ctx context.Context, data io.Reader, format Format, convert func(storage.Commit) (storage.Commit, error), destination string, mode storage.Durability) (report Verification, retErr error) {
	created := false
	complete := false
	var owned []string
	defer func() {
		if created && !complete {
			for _, name := range owned {
				if err := os.Remove(filepath.Join(destination, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
					retErr = errors.Join(retErr, err)
				}
			}
			retErr = errors.Join(retErr, os.Remove(destination))
		}
	}()
	if err := os.Mkdir(destination, 0o700); err != nil {
		return Verification{}, fmt.Errorf("create new migration directory: %w", err)
	}
	created = true
	lock, err := os.OpenFile(filepath.Join(destination, "writer.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return Verification{}, err
	}
	owned = append(owned, "writer.lock")
	locked := false
	defer func() {
		if locked {
			retErr = errors.Join(retErr, unlockFile(lock))
		}
		retErr = errors.Join(retErr, lock.Close())
	}()
	if err := lockFile(lock); err != nil {
		return Verification{}, err
	}
	locked = true
	out, err := os.OpenFile(filepath.Join(destination, "commits.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Verification{}, err
	}
	owned = append(owned, "commits.log")
	outClosed := false
	defer func() {
		if !outClosed {
			retErr = errors.Join(retErr, out.Close())
		}
	}()
	var end int64
	var epoch uint64
	for r := uint64(1); r <= format.Revision; r++ {
		if err := ctx.Err(); err != nil {
			return Verification{}, err
		}
		var h [headerSize]byte
		if _, err := io.ReadFull(data, h[:]); err != nil {
			return Verification{}, storage.ErrCorrupt
		}
		length := binary.BigEndian.Uint32(h[4:8])
		b := make([]byte, length)
		if _, err := io.ReadFull(data, b); err != nil {
			return Verification{}, storage.ErrCorrupt
		}
		var c storage.Commit
		if err := json.Unmarshal(b, &c); err != nil {
			return Verification{}, storage.ErrCorrupt
		}
		converted, err := convert(c)
		if err != nil {
			return Verification{}, fmt.Errorf("migrate commit %d: %w", r, err)
		}
		converted.Schema = CurrentSchema
		if err := state.ValidateCommit(converted, r, epoch); err != nil {
			return Verification{}, fmt.Errorf("migrated commit %d invalid: %w", r, err)
		}
		epoch = converted.Epoch
		payload, err := json.Marshal(converted)
		if err != nil {
			return Verification{}, err
		}
		if len(payload) > state.MaxCommitBytes {
			return Verification{}, fmt.Errorf("migrated commit %d exceeds the commit limit", r)
		}
		frame := make([]byte, headerSize)
		copy(frame, magic[:])
		binary.BigEndian.PutUint32(frame[4:8], uint32(len(payload)))
		binary.BigEndian.PutUint64(frame[8:16], r)
		binary.BigEndian.PutUint32(frame[16:20], crc32.Checksum(payload, table))
		binary.BigEndian.PutUint32(frame[20:24], crc32.Checksum(frame[:20], table))
		if err := writeAll(out, frame); err != nil {
			return Verification{}, err
		}
		if err := writeAll(out, payload); err != nil {
			return Verification{}, err
		}
		end += int64(len(frame) + len(payload))
	}
	if mode == storage.Strict {
		if err := durableSync(out); err != nil {
			return Verification{}, err
		}
	}
	outClosed = true
	if err := out.Close(); err != nil {
		return Verification{}, err
	}
	boundary, err := os.OpenFile(filepath.Join(destination, "boundary"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Verification{}, err
	}
	owned = append(owned, "boundary")
	if err := writeAll(boundary, boundaryMarker(format.Revision, end)); err != nil {
		boundary.Close()
		return Verification{}, err
	}
	if mode == storage.Strict {
		if err := durableSync(boundary); err != nil {
			boundary.Close()
			return Verification{}, err
		}
	}
	if err := boundary.Close(); err != nil {
		return Verification{}, err
	}
	if mode == storage.Strict {
		if err := syncDirectory(destination); err != nil {
			return Verification{}, err
		}
		if err := syncDirectory(filepath.Dir(destination)); err != nil {
			return Verification{}, err
		}
		if err := durableSync(lock); err != nil {
			return Verification{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Verification{}, err
	}
	complete = true
	return Verification{Valid: true, Scope: "journal", Schema: CurrentSchema, Revision: format.Revision, Epoch: epoch, CommittedBytes: end}, nil
}
