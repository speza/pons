package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestObservationExecutionContextRoundTrip(t *testing.T) {
	wire, err := json.Marshal(Observation{Workspace: "/remote/repo", Platform: "linux/arm64"})
	if err != nil {
		t.Fatal(err)
	}
	var got Observation
	if err := json.Unmarshal(wire, &got); err != nil {
		t.Fatal(err)
	}
	if got.Workspace != "/remote/repo" || got.Platform != "linux/arm64" {
		t.Fatalf("execution context = %+v", got)
	}
}

func TestActionArgsRemainTypedJSON(t *testing.T) {
	args, err := ArgsJSON(map[string]any{
		"count":   3,
		"enabled": true,
		"nested":  map[string]any{"items": []any{"x", nil}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := Action{ID: "a1", Kind: "typed", Args: args}
	wire, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Action
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(decoded.Args, &got); err != nil {
		t.Fatal(err)
	}
	if got["count"] != float64(3) || got["enabled"] != true {
		t.Fatalf("typed values were not preserved: %+v", got)
	}
	if !reflect.DeepEqual(got["nested"], map[string]any{"items": []any{"x", nil}}) {
		t.Fatalf("nested values changed: %#v", got["nested"])
	}
}

func TestActionArgumentHelpersHandleLegacyEmptyArgs(t *testing.T) {
	a := Action{Kind: "empty"}
	if got, err := ObjectArgs(a.Args); err != nil || len(got) != 0 {
		t.Fatalf("empty object: %#v %v", got, err)
	}
	var dst struct {
		Name string `json:"name"`
	}
	if err := a.DecodeArgs(&dst); err != nil {
		t.Fatal(err)
	}
	args, err := ArgsJSON(map[string]any{"name": 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := StringArg(args, "name"); err == nil {
		t.Fatal("StringArg should reject numeric coercion")
	}
}

func TestArgsJSONReportsMarshalErrors(t *testing.T) {
	if _, err := ArgsJSON(map[string]any{"bad": func() {}}); err == nil {
		t.Fatal("ArgsJSON must report marshal errors")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("MustArgsJSON should panic on marshal errors")
		}
	}()
	_ = MustArgsJSON(map[string]any{"bad": func() {}})
}
