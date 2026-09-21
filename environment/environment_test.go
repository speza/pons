package environment

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

type fakeSession struct{}

func (fakeSession) Catalog() []external.ToolDescription {
	return []external.ToolDescription{{
		Kind: "remote", Description: "remote tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
	}}
}
func (fakeSession) Metadata() Metadata { return Metadata{Provider: "fake"} }
func (fakeSession) Close() error       { return nil }
func (fakeSession) Execute(_ context.Context, action protocol.Action) (protocol.ToolResult, error) {
	return protocol.ToolResult{OK: true, Output: "ok", ActionID: action.ID, Kind: string(action.Kind)}, nil
}

func TestProxyRegistersSessionCatalog(t *testing.T) {
	core := pons.New()
	if err := core.Use(Proxy(fakeSession{})); err != nil {
		t.Fatal(err)
	}
	specs := core.ToolSpecs()
	if len(specs) != 1 || specs[0].Kind != "remote" || specs[0].Source.Executable != "fake" {
		t.Fatalf("specs = %+v", specs)
	}
	result, err := core.Execute(context.Background(), protocol.Action{ID: "one", Kind: "remote", Args: json.RawMessage(`{}`)})
	if err != nil || !result.OK || result.Output != "ok" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
}

func TestProxyPreflightsConflicts(t *testing.T) {
	core := pons.New()
	if err := core.AddTool("remote", pons.ToolDef{Handler: fakeSession{}.Execute}); err != nil {
		t.Fatal(err)
	}
	if err := core.Use(Proxy(fakeSession{})); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("conflict error = %v", err)
	}
}
