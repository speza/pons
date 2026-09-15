// Package protocol is the brain↔hands wire contract.
//
// Rules:
//   - JSON-serializable types only; no imports beyond stdlib.
//   - ActionKind is OPEN: the core defines only loop-control kinds.
//     Tool plugins define their own kinds (fs adds "read_file", etc.),
//     so capability vocabulary lives with the plugin that executes it.
package protocol

import (
	"encoding/json"
	"time"
)

// ActionKind identifies a capability. Core reserves loop-control kinds;
// plugins define the rest.
type ActionKind string

// ActFinish is the one core-reserved kind: the brain signalling "done".
const ActFinish ActionKind = "finish"

// Action is the unit of instruction from brain to hands.
type Action struct {
	ID     string            `json:"id"`
	Kind   ActionKind        `json:"kind"`
	Args   map[string]string `json:"args"`
	Danger Danger            `json:"danger,omitempty"`
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
	History   []TurnLog `json:"history,omitempty"`
	Now       time.Time `json:"now"`
}

// TurnLog is one completed round-trip (plan + results).
type TurnLog struct {
	Turn    int          `json:"turn"`
	Actions []Action     `json:"actions"`
	Results []ToolResult `json:"results"`
}
