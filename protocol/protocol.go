// Package protocol is the brain↔hands wire contract.
//
// Rules:
//   - JSON-serializable types only; no imports beyond stdlib.
//   - ActionKind is OPEN: the core defines only loop-control kinds.
//     Tool plugins define their own kinds (fs adds "read_file", etc.),
//     so capability vocabulary lives with the plugin that executes it.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// ActionKind identifies a capability. Core reserves loop-control kinds;
// plugins define the rest.
type ActionKind string

// ActFinish is the one core-reserved kind: the brain signalling "done".
const ActFinish ActionKind = "finish"

// Action is the unit of instruction from brain to hands.
//
// Args is an opaque JSON object.  It deliberately remains untyped at this
// boundary: the plugin which owns Kind owns the schema and decoding.  Keeping
// the original bytes (rather than converting through map[string]any) also
// preserves integers, nulls, arrays, nested objects, and the exact JSON
// vocabulary across a process boundary.
type Action struct {
	ID     string          `json:"id"`
	Kind   ActionKind      `json:"kind"`
	Args   json.RawMessage `json:"args"`
	Danger Danger          `json:"danger,omitempty"`
}

// ArgsJSON encodes an action argument object.  Callers which can handle
// construction errors should use this form; the wire carries the resulting
// JSON object unchanged.
func ArgsJSON(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode action args: %w", err)
	}
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	return b, nil
}

// MustArgsJSON is the explicit convenience form for package-level action
// constructors whose arguments are statically encodable. It panics on a
// programmer error instead of silently creating an invalid/nil action.
func MustArgsJSON(v any) json.RawMessage {
	b, err := ArgsJSON(v)
	if err != nil {
		panic(err)
	}
	return b
}

// DecodeArgs decodes an action's object into a plugin-owned type.  A missing
// Args value is treated as an empty object for compatibility with old action
// constructors which omitted optional arguments.
func (a Action) DecodeArgs(dst any) error {
	return DecodeArgs(a.Args, dst)
}

// DecodeArgs decodes raw action arguments into dst.  It rejects non-object JSON
// because tool_provider/v1 actions have object arguments.
func DecodeArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		raw = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	if object == nil {
		return fmt.Errorf("action args must be a JSON object")
	}
	return json.Unmarshal(raw, dst)
}

// ObjectArgs returns a copy of the members in raw.  Values remain raw JSON so
// callers can choose their own typed decoding without string coercion.
func ObjectArgs(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return map[string]json.RawMessage{}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("action args must be a JSON object")
	}
	return object, nil
}

// StringArg is a small compatibility helper for tools whose argument is a
// required or optional string.  Missing keys return an empty string; a value
// of another JSON type is an error instead of being silently stringified.
func StringArg(raw json.RawMessage, key string) (string, error) {
	object, err := ObjectArgs(raw)
	if err != nil {
		return "", err
	}
	value, ok := object[key]
	if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return "", nil
	}
	var out string
	if err := json.Unmarshal(value, &out); err != nil {
		return "", fmt.Errorf("argument %q must be a string: %w", key, err)
	}
	return out, nil
}

// Danger is the brain's advisory self-assessment. The harness does not
// trust it; policy lives at the execution boundary (in tool plugins or
// middleware), not in the brain's claims.
type Danger struct {
	RiskLevel string `json:"risk_level,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// ToolResult is the unit of observation from hands back to brain.
//
// Three audiences, one type:
//   - the brain observes via Observation() — the canonical model-facing
//     text, rendered once so every provider sees identical observations
//   - the loop reads OK for control flow (false = harness-level failure,
//     not a failed command; "exit 1" is OK:true + ExitCode, an observation)
//   - Kind/Payload is the structured result, owned by the producing plugin
//     (Kind = the action kind; consumers opt in via plugin accessors like
//     edit.AsEditResult). The core never interprets payloads.
type ToolResult struct {
	ActionID string          `json:"action_id"`
	OK       bool            `json:"ok"`
	Output   string          `json:"output,omitempty"`
	Error    string          `json:"error,omitempty"`
	ExitCode int             `json:"exit_code,omitempty"`
	Kind     string          `json:"kind,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// Observation is the canonical model-facing text for this result: the
// tool-rendered output plus the failure note when present. Brains must
// use this (not Output) so tool results are consistent across providers.
func (tr ToolResult) Observation() string {
	switch {
	case tr.Error == "":
		return tr.Output
	case tr.Output == "":
		return "error: " + tr.Error
	default:
		return tr.Output + "\nerror: " + tr.Error
	}
}

// Interpretation is the brain's verdict on a ToolResult.
type Interpretation struct {
	Continue   bool   `json:"continue"`
	Summary    string `json:"summary"`
	StopReason string `json:"stop_reason,omitempty"`
}

// Observation is an immutable snapshot handed to the brain each turn.
// Message is the user's current instruction — plain text, no framing label;
// the environment travels separately in the brain's rendering of it.
type Observation struct {
	Turn      int       `json:"turn"`
	Message   string    `json:"message"`
	Workspace string    `json:"workspace"`
	Platform  string    `json:"platform,omitempty"` // execution GOOS/GOARCH; empty means host-local
	History   []TurnLog `json:"history,omitempty"`
	Now       time.Time `json:"now"`
}

// TurnLog is one completed round-trip (response + results). Actions contains the
// complete plan, including a possible finish control action; Results contains
// only the non-finish actions that hands executed, in matching call order.
type TurnLog struct {
	Turn    int          `json:"turn"`
	Actions []Action     `json:"actions"`
	Results []ToolResult `json:"results"`
}
