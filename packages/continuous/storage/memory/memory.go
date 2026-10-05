// Package memory provides ephemeral continuous storage with transactional semantics.
package memory

import (
	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/internal/state"
)

func Open() storage.Store {
	s, err := state.New(storage.Capabilities{Durability: storage.Memory}, nil, nil, nil)
	if err != nil {
		panic(err)
	}
	return s
}
