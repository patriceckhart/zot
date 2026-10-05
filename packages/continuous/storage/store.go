// Package storage defines the experimental continuous storage contract.
// Callers perform external work outside commits. Values are opaque JSON records.
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrClosed   = errors.New("continuous store closed")
	ErrConflict = errors.New("continuous revision conflict")
	ErrFenced   = errors.New("continuous writer fenced")
	ErrLocked   = errors.New("continuous store already has a writer")
	ErrCorrupt  = errors.New("continuous journal corrupt")
	ErrCursor   = errors.New("continuous cursor outside retained history")
)

type Durability string

const (
	Memory  Durability = "memory"
	Process Durability = "process"
	Strict  Durability = "strict"
)

// Capabilities are explicit rather than inferred from the backend name.
type Capabilities struct {
	Durability           Durability `json:"durability"`
	Indexed              bool       `json:"indexed"`
	BoundedHistoryMemory bool       `json:"bounded_history_memory"`
}

type Record struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type Operation struct {
	Key    string          `json:"key"`
	Value  json.RawMessage `json:"value,omitempty"`
	Delete bool            `json:"delete,omitempty"`
}

type Commit struct {
	Schema     int         `json:"schema"`
	Revision   uint64      `json:"revision"`
	Epoch      uint64      `json:"epoch"`
	Time       time.Time   `json:"time"`
	Actor      string      `json:"actor,omitempty"`
	Operations []Operation `json:"operations"`
}

// Mutation uses a mandatory expected revision and writer epoch. A successful
// acknowledgement means the entire commit passed the backend persistence barrier.
type Mutation struct {
	ExpectedRevision uint64
	Epoch            uint64
	Actor            string
	Operations       []Operation
}

// Snapshot is detached from the store. Records and history are paginated
// separately so clients do not need an unbounded subscription buffer.
type Snapshot interface {
	Revision() uint64
	Get(key string) (json.RawMessage, bool)
	Page(prefix, after string, limit int) ([]Record, error)
}

// Store has one writer per open instance. Scan plus Wait provides a gap-free
// pull subscription: Wait returns immediately if a newer revision already exists.
// Cancelling Wait never cancels admitted work. Close unblocks every waiter.
// This initial contract is provisional, not a stable public SDK.
type Store interface {
	Capabilities() Capabilities
	Epoch() uint64
	Snapshot(context.Context) (Snapshot, error)
	Commit(context.Context, Mutation) (Commit, error)
	Scan(context.Context, uint64, int) ([]Commit, error)
	Wait(context.Context, uint64) error
	Close() error
}
