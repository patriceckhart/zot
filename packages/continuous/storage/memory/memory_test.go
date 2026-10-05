package memory

import (
	"testing"

	"github.com/patriceckhart/zot/packages/continuous/storage"
	"github.com/patriceckhart/zot/packages/continuous/storage/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(*testing.T) storage.Store { return Open() })
}
