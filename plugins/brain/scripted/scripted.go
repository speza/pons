// Package scripted is a deterministic brain plugin — a stand-in for an
// LLM-driven brain that lets the whole harness run in CI with no API keys.
// It walks a fixed script of steps; when the script is exhausted it finishes.
package scripted

import (
	"context"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

// Step is one planned round of actions plus a reason.
type Step struct {
	Reason  string
	Actions []protocol.Action
}

// Scripted is the brain implementation.
type Scripted struct {
	Steps []Step
	i     int
}

// New creates the brain plugin (installed with pons.Core.Use).
func New(steps ...Step) *Scripted {
	return &Scripted{Steps: steps}
}

// Setup installs the brain into the core.
func (s *Scripted) Setup(c *pons.Core) error {
	return c.SetBrain(s)
}

func (s *Scripted) Respond(ctx context.Context, obs protocol.Observation) (pons.AssistantResponse, error) {
	if s.i >= len(s.Steps) {
		return pons.AssistantResponse{Actions: []protocol.Action{pons.Finish("done — script exhausted")}}, nil
	}
	st := s.Steps[s.i]
	s.i++
	return pons.AssistantResponse{Actions: st.Actions}, nil
}

func (s *Scripted) Interpret(ctx context.Context, obs protocol.Observation, tr protocol.ToolResult) (protocol.Interpretation, error) {
	if !tr.OK {
		return protocol.Interpretation{Continue: false, StopReason: "tool_failed", Summary: tr.Error}, nil
	}
	return protocol.Interpretation{Continue: true, Summary: tr.Output}, nil
}

func (s *Scripted) Close(ctx context.Context) error { return nil }
