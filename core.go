// Package pons is a minimal, plugin-extensible agent harness with a hard
// brain/hands separation.
//
// The core knows three things and nothing else:
//
//   - protocol/: the wire types between brain and hands (no logic)
//   - the two ports: ControlPort (what a brain must speak) and ToolPort
//     (what hands must speak)
//   - the turn loop and a plugin seam for ADDING capabilities
//
// A fresh Core has zero capabilities — no tools, no brain. Plugins purely
// add functionality (tools, brains, middleware, persistence hooks); the
// core never imports a plugin. Typical main():
//
//	core := pons.New()
//	core.Use(fs.New(fs.Config{Root: ws}), shell.New(cfg), sessionsjsonl.New(dir))
//	core.Use(brainscripted.New(steps...))
//	core.Run(ctx)
package pons

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/samperrin/pons/protocol"
)

// ControlPort is what a brain must speak. It sees Observations, emits
// Actions; it never executes anything.
type ControlPort interface {
	// NextActions plans the next round of actions for this observation.
	NextActions(ctx context.Context, obs protocol.Observation) ([]protocol.Action, error)
	// Interpret reflects on a completed tool result.
	Interpret(ctx context.Context, obs protocol.Observation, tr protocol.ToolResult) (protocol.Interpretation, error)
	// Close releases brain resources.
	Close(ctx context.Context) error
}

// ToolPort is what hands must speak: execute one approved action.
type ToolPort interface {
	Execute(ctx context.Context, a protocol.Action) (protocol.ToolResult, error)
}

// ToolHandler executes one action kind. Tool plugins provide these.
type ToolHandler func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error)

// TurnHook observes one completed turn (plan + results). Observational hooks
// cannot fail the run; use TurnErrorHook for persistence that must be checked.
type TurnHook func(obs protocol.Observation, turn protocol.TurnLog)

// TurnErrorHook observes one completed turn and can fail the run. Persistence
// hooks should use this form so a durable-write failure is not silently lost.
type TurnErrorHook func(obs protocol.Observation, turn protocol.TurnLog) error

type turnHook func(obs protocol.Observation, turn protocol.TurnLog) error

// Plugin adds capabilities to a Core. Purely additive by contract:
// Setup only registers; it must not change existing registrations.
type Plugin interface {
	Setup(*Core) error
}

// Core is the minimal harness. Zero capabilities until plugins are applied.
type Core struct {
	// brain is the control layer. It is installed only through SetBrain so
	// duplicate brain registration cannot bypass the additive contract.
	brain ControlPort

	// Workspace / MaxTurns / Log tune the loop.
	Workspace string
	MaxTurns  int
	Log       *log.Logger

	handlers map[protocol.ActionKind]ToolHandler
	specs    map[protocol.ActionKind]ToolSpec
	wraps    []func(ToolPort) ToolPort
	onTurns  []turnHook
	onEvents []func(Event)
}

func New() *Core {
	return &Core{
		handlers: make(map[protocol.ActionKind]ToolHandler),
		specs:    make(map[protocol.ActionKind]ToolSpec),
	}
}

// Use applies plugins in order, stopping at the first failure.
func (c *Core) Use(ps ...Plugin) error {
	for _, p := range ps {
		if err := p.Setup(c); err != nil {
			return fmt.Errorf("plugin setup: %w", err)
		}
	}
	return nil
}

// ToolParam describes one argument of an action kind for the legacy compact
// schema builder.  New cross-process tools should provide InputSchema instead;
// handlers still own typed decoding.
type ToolParam struct {
	Name        string
	Type        string // "string", "integer", "boolean"
	Description string
	Required    bool
}

// ToolSpec is the brain-facing description of a registered action kind.
type ToolSpec struct {
	Kind        protocol.ActionKind
	Description string
	Params      []ToolParam
	// InputSchema is a JSON-Schema object for typed tools.  It is kept as raw
	// JSON so the core does not own a capability vocabulary or schema dialect.
	InputSchema json.RawMessage
	Source      ToolSource
}

// ToolSource identifies the provider behind a registered tool. Built-in tools
// leave this at its zero value; external adapters populate it for presentation
// and audit without changing the Action/ToolResult wire contract.
type ToolSource struct {
	PluginName        string
	PluginVersion     string
	Capability        string
	CapabilityVersion int
	Executable        string
	External          bool
}

// ToolDef is what a tool plugin registers: the executor plus its schema.
type ToolDef struct {
	Handler     ToolHandler
	Description string
	Params      []ToolParam
	InputSchema json.RawMessage
	Source      ToolSource
}

// AddTool registers a handler for an action kind. Enforcement of the
// purely-additive contract: registering an already-registered kind is an
// error, so plugin conflicts fail loudly at composition time instead of
// silently last-wins.
func (c *Core) AddTool(kind protocol.ActionKind, def ToolDef) error {
	if kind == "" {
		return errors.New("pons: action kind must not be empty")
	}
	if kind == protocol.ActFinish {
		return fmt.Errorf("pons: action kind %q is reserved for the core", kind)
	}
	if def.Handler == nil {
		return fmt.Errorf("pons: action kind %q has a nil handler", kind)
	}
	if _, exists := c.handlers[kind]; exists {
		return fmt.Errorf("pons: action kind %q already registered (plugin conflict)", kind)
	}
	c.handlers[kind] = def.Handler
	c.specs[kind] = ToolSpec{Kind: kind, Description: def.Description, Params: def.Params, InputSchema: append(json.RawMessage(nil), def.InputSchema...), Source: def.Source}
	return nil
}

// HasTool reports whether an action kind is already registered. It is useful
// to preflight a multi-tool plugin before mutating the additive registry.
func (c *Core) HasTool(kind protocol.ActionKind) bool {
	_, ok := c.handlers[kind]
	return ok
}

// ToolSpecs returns the registered action kinds in stable (sorted) order —
// the vocabulary an LLM-driven brain presents to the model.
func (c *Core) ToolSpecs() []ToolSpec {
	out := make([]ToolSpec, 0, len(c.specs))
	for _, s := range c.specs {
		s.InputSchema = append(json.RawMessage(nil), s.InputSchema...)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// WrapTool appends middleware around the execution chain. Middleware sees
// every action before its handler runs; approval/audit plugins use this.
func (c *Core) WrapTool(w func(ToolPort) ToolPort) {
	c.wraps = append(c.wraps, w)
}

// SetBrain installs the brain. Refusing a second brain keeps composition
// explicit — a confused agent is worse than a failed startup.
func (c *Core) SetBrain(b ControlPort) error {
	if c.brain != nil {
		return errors.New("pons: brain already set")
	}
	if b == nil {
		return errors.New("pons: cannot install a nil brain")
	}
	c.brain = b
	return nil
}

// OnTurn appends an observational hook for every completed turn. Errors from
// this form are intentionally not possible; checked persistence should use
// OnTurnError.
func (c *Core) OnTurn(h TurnHook) {
	c.onTurns = append(c.onTurns, func(obs protocol.Observation, turn protocol.TurnLog) error {
		h(obs, turn)
		return nil
	})
}

// OnTurnError appends a persistence/audit hook whose error is propagated from
// Run. Hooks run in registration order with ordinary OnTurn observers.
func (c *Core) OnTurnError(h TurnErrorHook) {
	c.onTurns = append(c.onTurns, turnHook(h))
}

// EventType names one stage of the agent loop (pi-style names).
type EventType string

const (
	EventAgentStart  EventType = "agent_start"
	EventTurnStart   EventType = "turn_start"
	EventActionStart EventType = "action_start"
	EventActionEnd   EventType = "action_end"
	EventTurnEnd     EventType = "turn_end"
	EventFinish      EventType = "finish"
	EventStopped     EventType = "stopped"
	EventExhausted   EventType = "exhausted"
)

// Event is one observable step of the loop.
type Event struct {
	Type    EventType
	Turn    int
	Text    string               // narration: finish reason, stop reason, errors
	Action  *protocol.Action     // set on action_start / action_end
	Result  *protocol.ToolResult // set on action_end
	Tool    *ToolSpec            // registered tool metadata on action_start / action_end
	Actions []protocol.Action    // set on turn_end (whole plan)
	Results []protocol.ToolResult
}

// OnEvent subscribes to the loop's event stream. Multiple subscribers
// compose; UIs, loggers, and metrics plugins all attach here.
func (c *Core) OnEvent(fn func(Event)) {
	c.onEvents = append(c.onEvents, fn)
}

func (c *Core) emit(e Event) {
	for _, fn := range c.onEvents {
		fn(e)
	}
}

// RunResult is the outcome of one Run.
type RunResult struct {
	// Answer is the brain's final text (the finish reason). Empty when the
	// loop stopped early or exhausted its budget.
	Answer string
	// Turns executed.
	Turns int
	// Exhausted is true when MaxTurns ran out without a finish signal.
	Exhausted bool
	// History is the per-turn audit trail.
	History []protocol.TurnLog
}

// Run executes one full task: plan → act → reflect → repeat, until the
// brain finishes (text-only / finish action) or MaxTurns is exhausted.
func (c *Core) Run(ctx context.Context, message string) (RunResult, error) {
	if c.brain == nil {
		return RunResult{}, errors.New("pons: no brain plugin installed")
	}
	brain := c.brain
	port := c.buildToolPort()

	obs := protocol.Observation{Message: message, Workspace: c.Workspace}
	result := RunResult{}
	maxTurns := c.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 25
	}
	c.emit(Event{Type: EventAgentStart, Text: message})

	for turn := 1; turn <= maxTurns; turn++ {
		obs.Turn, obs.Now = turn, time.Now()
		c.emit(Event{Type: EventTurnStart, Turn: turn})

		actions, err := brain.NextActions(ctx, obs)
		if err != nil {
			return result, fmt.Errorf("brain plan (turn %d): %w", turn, err)
		}
		if len(actions) == 0 {
			return result, fmt.Errorf("brain returned no actions at turn %d", turn)
		}

		var results []protocol.ToolResult
		var answer string
		finished := false
		// Everything before a finish action executes — concurrently. The
		// model emits independent calls in one turn; serializing them would
		// waste latency. Determinism is preserved where it matters:
		// starts are emitted in call order, results are indexed by call
		// order (never completion order), and ActionEnd/turn logs replay
		// in call order after the last completes.
		run, finishIdx := actions, -1
		for i, a := range actions {
			if a.Kind == protocol.ActFinish {
				finishIdx = i
				break
			}
		}
		if finishIdx >= 0 {
			run, finished = actions[:finishIdx], true
			answer, _ = protocol.StringArg(actions[finishIdx].Args, "reason")
			c.logf("[turn %d] brain signalled finish: %s", turn, answer)
		}

		for i := range run {
			a := run[i]
			c.emit(Event{Type: EventActionStart, Turn: turn, Action: &a, Tool: c.toolSpec(a.Kind)})
		}
		results = make([]protocol.ToolResult, len(run))
		var wg sync.WaitGroup
		for i, a := range run {
			wg.Add(1)
			go func(i int, a protocol.Action) {
				defer wg.Done()
				tr, err := executeTool(port, ctx, a)
				if err != nil { // handler crash ≠ observation; contain it
					tr = protocol.ToolResult{ActionID: a.ID, OK: false, Error: err.Error()}
				}
				if tr.ActionID == "" {
					tr.ActionID = a.ID
				}
				results[i] = tr
			}(i, a)
		}
		wg.Wait()
		for i := range run {
			a, tr := run[i], results[i]
			c.emit(Event{Type: EventActionEnd, Turn: turn, Action: &a, Result: &tr, Tool: c.toolSpec(a.Kind)})
			c.logf("[turn %d] ← %s ok=%v err=%q", turn, a.Kind, tr.OK, tr.Error)
		}

		turnLog := protocol.TurnLog{Turn: turn, Actions: actions, Results: results}
		result.Turns, result.History = turn, append(result.History, turnLog)
		for _, h := range c.onTurns {
			if err := h(obs, turnLog); err != nil {
				return result, fmt.Errorf("turn hook (turn %d): %w", turn, err)
			}
		}
		c.emit(Event{Type: EventTurnEnd, Turn: turn, Actions: actions, Results: results})

		if finished {
			result.Answer = answer
			c.emit(Event{Type: EventFinish, Turn: turn, Text: answer})
			return result, nil
		}

		var stopReason string
		stopped := false
		for _, tr := range results {
			interp, err := brain.Interpret(ctx, obs, tr)
			if err != nil {
				return result, fmt.Errorf("brain interpret (turn %d): %w", turn, err)
			}
			if !interp.Continue && !stopped {
				stopped = true
				stopReason = interp.StopReason
			}
		}
		obs.History = append(obs.History, turnLog)
		if stopped {
			c.logf("[turn %d] brain stopped: %s", turn, stopReason)
			c.emit(Event{Type: EventStopped, Turn: turn, Text: stopReason})
			return result, nil
		}
	}
	result.Exhausted = true
	c.emit(Event{Type: EventExhausted, Turn: maxTurns, Text: fmt.Sprintf("exhausted %d turns", maxTurns)})
	return result, nil
}

func (c *Core) toolSpec(kind protocol.ActionKind) *ToolSpec {
	spec, ok := c.specs[kind]
	if !ok {
		return nil
	}
	spec.InputSchema = append(json.RawMessage(nil), spec.InputSchema...)
	return &spec
}

// buildToolPort composes the dispatch handler with registered middleware
// (outermost wrap = last registered, so middleware order reads top-down).
func (c *Core) buildToolPort() ToolPort {
	var port ToolPort = dispatchFunc(func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
		h, ok := c.handlers[a.Kind]
		if !ok {
			// Unknown capability is an observation, not a crash: the
			// brain sees it and can adapt.
			return protocol.ToolResult{ActionID: a.ID, OK: false,
				Error: fmt.Sprintf("no plugin provides action kind %q", a.Kind), Kind: string(a.Kind)}, nil
		}
		tr, err := h(ctx, a)
		// Result payloads are namespaced by the action kind that produced
		// them; plugins may not even set it.
		if tr.Kind == "" {
			tr.Kind = string(a.Kind)
		}
		return tr, err
	})
	for _, wrap := range slices.Backward(c.wraps) {
		port = wrap(port)
	}
	return port
}

type dispatchFunc func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error)

func (f dispatchFunc) Execute(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	return f(ctx, a)
}

func executeTool(port ToolPort, ctx context.Context, a protocol.Action) (tr protocol.ToolResult, err error) {
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("tool panic: %v", v)
		}
	}()
	return port.Execute(ctx, a)
}

// Finish is the core-reserved action constructor.
func Finish(reason string) protocol.Action {
	return protocol.Action{Kind: protocol.ActFinish, Args: protocol.MustArgsJSON(map[string]string{"reason": reason})}
}

func (c *Core) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log.Printf(format, args...)
	}
}
