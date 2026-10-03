package sdk

import (
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestAuxiliaryUsageEvent(t *testing.T) {
	for _, auxiliary := range []bool{false, true} {
		event := toEvent(core.EvUsage{Auxiliary: auxiliary, Usage: provider.Usage{InputTokens: 3}, Cumulative: provider.Usage{InputTokens: 103}})
		if event.Auxiliary != auxiliary || event.Usage.Input != 3 || event.Cumulative.Input != 103 {
			t.Fatalf("event=%+v", event)
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		if value, exists := wire["auxiliary"]; exists != auxiliary || auxiliary && value != true {
			t.Fatalf("wire=%s", encoded)
		}
	}
}
