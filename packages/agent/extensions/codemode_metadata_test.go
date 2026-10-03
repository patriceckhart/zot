package extensions

import (
	"encoding/json"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
)

func TestExtensionCodemodeMetadataReachesRegistry(t *testing.T) {
	info := ToolInfo{Extension: "synthetic", Name: "query", Schema: json.RawMessage(`{"type":"object"}`), OutputSchema: json.RawMessage(`{"type":"object"}`), Namespace: "support", NamespaceDescription: "Customer service", NamespaceInstructions: "Escalate", Exposure: "codemode"}
	spec := core.NewRegistry(NewTool(nil, info)).AllSpecs()[0]
	if spec.Namespace != "support" || spec.NamespaceDescription != "Customer service" || spec.NamespaceInstructions != "Escalate" || spec.Exposure != "codemode" || !spec.Deferred || string(spec.OutputSchema) != `{"type":"object"}` {
		t.Fatalf("spec: %+v", spec)
	}
	legacy := core.NewRegistry(NewTool(nil, ToolInfo{Extension: "synthetic", Name: "legacy", Deferred: true})).AllSpecs()[0]
	if legacy.Namespace != "synthetic" || legacy.Exposure != "deferred" || !legacy.Deferred {
		t.Fatalf("legacy: %+v", legacy)
	}
}
