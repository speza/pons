// Package pons is a minimal, plugin-extensible agent harness with a hard
// brain/hands separation.
//
// The core knows three things and nothing else:
//
//   - protocol/: the wire types between brain and hands (no logic)
//   - the two ports: ControlPort (what a brain must speak) and ToolPort
//     (what hands must speak)
//   - the step loop and a plugin seam for ADDING capabilities
//
// A fresh Core has zero capabilities — no tools, no brain. Plugins purely
// add functionality (tools, brains, middleware, observers); the
// core never imports a plugin. Typical main():
//
//	core := pons.New()
//	core.Use(fs.New(fs.Config{Root: ws}), shell.New(cfg))
//	core.Use(brainscripted.New(steps...))
//	core.Run(ctx)
package pons

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/samperrin/pons/protocol"
)

// ControlPort is what a brain must speak. It sees Observations, produces
// assistant responses, and never executes actions itself.
type ControlPort interface {
	// Respond produces the assistant response for this observation.
	Respond(ctx context.Context, obs protocol.Observation) (AssistantResponse, error)
	// Interpret reflects on a completed tool result.
	Interpret(ctx context.Context, obs protocol.Observation, tr protocol.ToolResult) (protocol.Interpretation, error)
	// Close releases brain resources.
	Close(ctx context.Context) error
}

type AssistantPartType string

const (
	AssistantPartText     AssistantPartType = "text"
	AssistantPartToolCall AssistantPartType = "tool_call"
)

// AssistantPart is one ordered provider-neutral block in an assistant response.
// Action is populated for AssistantPartToolCall.
type AssistantPart struct {
	Type   AssistantPartType
	Text   string
	Action protocol.Action
}

// AssistantResponse is the complete output produced by a brain for one step.
// Parts preserve its provider-neutral content; Actions are the tool calls the
// core should execute.
type AssistantResponse struct {
	Parts   []AssistantPart
	Actions []protocol.Action
}

// ToolPort is what hands must speak: execute one approved action.
type ToolPort interface {
	Execute(ctx context.Context, a protocol.Action) (protocol.ToolResult, error)
}

// Execute dispatches one action through the configured hands middleware and
// tool registry. It is the embedding seam used by a standalone tool host and
// does not run tool-call-start hooks. Ordinary agents should call Run so
// planning, preflight checks, and interpretation remain owned by the core loop.
func (c *Core) Execute(ctx context.Context, a protocol.Action) (protocol.ToolResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return executeTool(c.buildToolPort(), ctx, a)
}

// ToolHandler executes one action kind. Tool plugins provide these.
type ToolHandler func(ctx context.Context, a protocol.Action) (protocol.ToolResult, error)

// StepHook observes one completed step (response + results). Observational hooks
// cannot fail the run; use StepErrorHook for persistence that must be checked.
type StepHook func(obs protocol.Observation, step protocol.StepLog)

// StepErrorHook observes one completed step and can fail the run. Persistence
// hooks should use this form so a durable-write failure is not silently lost.
type StepErrorHook func(obs protocol.Observation, step protocol.StepLog) error

type stepHook func(obs protocol.Observation, step protocol.StepLog) error

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

	// Workspace and Platform describe the hands execution environment.
	Workspace           string
	Platform            string
	ActionEnvironment   ActionEnvironment
	RecentActionContext []ActionContextItem
	MaxSteps            int
	Log                 *log.Logger

	handlers      map[protocol.ActionKind]ToolHandler
	specs         map[protocol.ActionKind]ToolSpec
	wraps         []func(ToolPort) ToolPort
	onSteps       []stepHook
	onEvents      []func(Event)
	onEventErrors []func(Event) error
	hooks         []Hooks
	approver      ApprovalHandler
	resources     map[protocol.ActionKind]ResourceProjection
	capabilities  map[string]any
}

func New() *Core {
	return &Core{
		handlers:     make(map[protocol.ActionKind]ToolHandler),
		specs:        make(map[protocol.ActionKind]ToolSpec),
		resources:    make(map[protocol.ActionKind]ResourceProjection),
		capabilities: make(map[string]any),
	}
}

// RegisterCapability exposes a trusted plugin capability to other plugins.
// Names are unique; registration never replaces an existing provider.
func (c *Core) RegisterCapability(name string, value any) error {
	if name == "" {
		return errors.New("pons: capability name must not be empty")
	}
	if value == nil {
		return fmt.Errorf("pons: capability %q is nil", name)
	}
	valueOf := reflect.ValueOf(value)
	switch valueOf.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if valueOf.IsNil() {
			return fmt.Errorf("pons: capability %q is nil", name)
		}
	}
	if _, exists := c.capabilities[name]; exists {
		return fmt.Errorf("pons: capability %q already registered (plugin conflict)", name)
	}
	if c.capabilities == nil {
		c.capabilities = make(map[string]any)
	}
	c.capabilities[name] = value
	return nil
}

// Capability resolves a capability registered by a trusted plugin.
func (c *Core) Capability(name string) (any, bool) {
	value, ok := c.capabilities[name]
	return value, ok
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
	Resources   ResourceProjection
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
	if def.Resources != nil {
		c.resources[kind] = def.Resources
	}
	c.specs[kind] = ToolSpec{
		Kind:        kind,
		Description: def.Description,
		Params:      append([]ToolParam(nil), def.Params...),
		InputSchema: append(json.RawMessage(nil), def.InputSchema...),
		Source:      def.Source,
	}
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
		s.Params = append([]ToolParam(nil), s.Params...)
		s.InputSchema = append(json.RawMessage(nil), s.InputSchema...)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// WrapTool appends middleware around the execution chain. Middleware sees
// every action that passed tool-call-start hooks before its handler runs.
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

// OnStep appends an observational hook for every completed step. Errors from
// this form are intentionally not possible; checked persistence should use
// OnStepError.
func (c *Core) OnStep(h StepHook) {
	c.onSteps = append(c.onSteps, func(obs protocol.Observation, step protocol.StepLog) error {
		h(obs, step)
		return nil
	})
}

// OnStepError appends a persistence/audit hook whose error is propagated from
// Run. Hooks run in registration order with ordinary OnStep observers.
func (c *Core) OnStepError(h StepErrorHook) {
	c.onSteps = append(c.onSteps, stepHook(h))
}

// EventType names one stage of the agent loop.
type EventType string

const (
	EventAgentStart        EventType = "agent_start"
	EventStepStart         EventType = "step_start"
	EventAssistantResponse EventType = "assistant_response"
	EventActionStart       EventType = "action_start"
	EventActionDecision    EventType = "action_decision"
	EventApprovalRequest   EventType = "approval_request"
	EventApprovalResolved  EventType = "approval_resolved"
	EventActionDenied      EventType = "action_denied"
	EventActionEnd         EventType = "action_end"
	EventStepEnd           EventType = "step_end"
	EventFinish            EventType = "finish"
	EventStopped           EventType = "stopped"
	EventExhausted         EventType = "exhausted"
)

// Event is one observable step of the loop.
type Event struct {
	Type     EventType
	Step     int
	Text     string               // narration: finish reason, stop reason, errors
	Action   *protocol.Action     // set on action-specific events
	Result   *protocol.ToolResult // set on action_denied / action_end
	Tool     *ToolSpec            // registered metadata on action-specific events
	Actions  []protocol.Action    // set on assistant_response and step_end
	Results  []protocol.ToolResult
	Parts    []AssistantPart // set on assistant_response (ordered assistant content)
	Decision *ActionDecision // set on decision/approval/denial events
}

// OnEvent subscribes to the loop's event stream. Multiple subscribers
// compose; UIs, loggers, and metrics plugins all attach here.
func (c *Core) OnEvent(fn func(Event)) {
	c.onEvents = append(c.onEvents, fn)
}

// OnEventError subscribes a checked event sink. The core stops before the
// next side effect when a sink fails; runtimes use this to make tool-call
// intent durable before execution begins.
func (c *Core) OnEventError(fn func(Event) error) {
	c.onEventErrors = append(c.onEventErrors, fn)
}

func (c *Core) emit(e Event) error {
	for _, fn := range c.onEvents {
		fn(e)
	}
	for _, fn := range c.onEventErrors {
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// RunResult is the outcome of one Run.
type RunResult struct {
	// Answer is the brain's final text (the finish reason). Empty when the
	// loop stopped early or exhausted its budget.
	Answer string
	// Steps executed.
	Steps int
	// Exhausted is true when MaxSteps ran out without a finish signal.
	Exhausted bool
	// History is the per-step audit trail.
	History []protocol.StepLog
}

// Run executes one full task: respond → act → reflect → repeat, until the
// brain finishes (text-only / finish action) or MaxSteps is exhausted.
func (c *Core) Run(ctx context.Context, message string) (result RunResult, runErr error) {
	if c.brain == nil {
		return RunResult{}, errors.New("pons: no brain plugin installed")
	}
	brain := c.brain
	port := c.buildToolPort()

	obs := protocol.Observation{Message: message, Workspace: c.Workspace, Platform: c.Platform}
	denials := make(map[string]int)
	recentContext := boundedActionContext(c.RecentActionContext)
	maxSteps := c.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 25
	}
	currentStep := 0
	endReason := AgentEndReason("")
	agentStopReason := ""
	agentOpen := false
	stepOpen := false
	defer func() {
		if stepOpen {
			if runErr != nil {
				event := AgentStepErrorEvent{Step: currentStep, Err: runErr}
				hookErr := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *AgentStepErrorEvent) error {
					return h.OnAgentStepError
				}, &event)
				if event.Err != nil {
					runErr = event.Err
				}
				if hookErr != nil {
					runErr = errors.Join(runErr, hookErr)
				}
			}
			event := AgentStepEndEvent{Step: currentStep, Run: result}
			if err := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *AgentStepEndEvent) error {
				return h.OnAgentStepEnd
			}, &event); err != nil {
				runErr = errors.Join(runErr, err)
			}
		}
		if agentOpen {
			if runErr != nil {
				event := AgentErrorEvent{Step: currentStep, Err: runErr}
				hookErr := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *AgentErrorEvent) error {
					return h.OnAgentError
				}, &event)
				if event.Err != nil {
					runErr = event.Err
				}
				if hookErr != nil {
					runErr = errors.Join(runErr, hookErr)
				}
			}
			if runErr != nil {
				endReason = AgentEndFailed
			}
			event := AgentEndEvent{Result: result, Err: runErr, Reason: endReason, StopReason: agentStopReason}
			if err := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *AgentEndEvent) error {
				return h.OnAgentEnd
			}, &event); err != nil {
				runErr = errors.Join(runErr, err)
			}
			result = event.Result
		}
	}()
	if err := c.emit(Event{Type: EventAgentStart, Text: message}); err != nil {
		return result, fmt.Errorf("event agent_start: %w", err)
	}
	agentOpen = true
	agentStart := AgentStartEvent{Message: message, Workspace: c.Workspace, Platform: c.Platform}
	if err := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *AgentStartEvent) error {
		return h.OnAgentStart
	}, &agentStart); err != nil {
		return result, fmt.Errorf("agent start: %w", err)
	}
	message = agentStart.Message
	obs.Message = message

	for step := 1; step <= maxSteps; step++ {
		obs.Step, obs.Now = step, time.Now()
		if err := c.emit(Event{Type: EventStepStart, Step: step}); err != nil {
			return result, fmt.Errorf("event step_start: %w", err)
		}
		currentStep, stepOpen = step, true
		stepStart := AgentStepStartEvent{Observation: obs}
		if err := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *AgentStepStartEvent) error {
			return h.OnAgentStepStart
		}, &stepStart); err != nil {
			return result, fmt.Errorf("agent step start: %w", err)
		}
		obs = stepStart.Observation
		obs.Step, obs.Workspace, obs.Platform = step, c.Workspace, c.Platform

		response, err := brain.Respond(ctx, obs)
		if err != nil {
			return result, fmt.Errorf("brain response (step %d): %w", step, err)
		}
		responseEvent := AssistantResponseEvent{Step: step, Response: response}
		if err := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *AssistantResponseEvent) error {
			return h.OnAssistantResponse
		}, &responseEvent); err != nil {
			return result, fmt.Errorf("assistant response (step %d): %w", step, err)
		}
		response = responseEvent.Response
		actions := response.Actions
		if len(actions) == 0 {
			return result, fmt.Errorf("brain returned no actions at step %d", step)
		}
		for _, part := range response.Parts {
			if part.Type == AssistantPartText {
				recentContext = boundedActionContext(append(recentContext, ActionContextItem{
					Source: ContextAssistant, Text: part.Text,
				}))
			}
		}

		var results []protocol.ToolResult
		var answer string
		finished := false
		// Everything before a finish action executes — concurrently. The
		// model emits independent calls in one step; serializing them would
		// waste latency. Determinism is preserved where it matters:
		// starts are emitted in call order, results are indexed by call
		// order (never completion order), and ActionEnd/step logs replay
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
			c.logf("[step %d] brain signalled finish: answer_chars=%d", step, len(answer))
		}
		var parts []AssistantPart
		if len(run) > 0 {
			parts, err = normalizeAssistantParts(response.Parts, run)
			if err != nil {
				return result, fmt.Errorf("brain response (step %d): %w", step, err)
			}
		}

		approved := make([]bool, len(run))
		decisions := make([]*ActionDecision, len(run))
		results = make([]protocol.ToolResult, len(run))
		for i := range run {
			a := run[i]
			decision, action, err := c.evaluateToolCallStart(ctx, ToolCallStartEvent{
				Step: step, Message: message, Workspace: c.Workspace,
				Platform: c.Platform, Environment: c.ActionEnvironment,
				Action: a, Tool: c.toolSpec(a.Kind), RecentContext: boundedActionContext(recentContext),
			}, denials)
			if err != nil {
				return result, fmt.Errorf("tool call start (step %d): %w", step, err)
			}
			run[i] = action
			if decision != nil && decision.Action == DispositionDeny {
				decisions[i] = decision
				results[i] = deniedResult(action, decision.ReasonCode)
				continue
			}
			approved[i] = true
		}
		if len(run) > 0 {
			if len(response.Parts) == 0 {
				for i := range run {
					parts[i].Action = run[i]
				}
			} else {
				byID := make(map[string]protocol.Action, len(run))
				for _, action := range run {
					byID[action.ID] = action
				}
				for i := range parts {
					if parts[i].Type == AssistantPartToolCall {
						parts[i].Action = byID[parts[i].Action.ID]
					}
				}
			}
			if err := c.emit(Event{Type: EventAssistantResponse, Step: step, Actions: slices.Clone(run), Parts: parts}); err != nil {
				return result, fmt.Errorf("event assistant_response: %w", err)
			}
		}
		for i := range run {
			if !approved[i] {
				continue
			}
			a := run[i]
			if err := c.emit(Event{Type: EventActionStart, Step: step, Action: &a, Tool: c.toolSpec(a.Kind)}); err != nil {
				return result, fmt.Errorf("event action_start: %w", err)
			}
		}

		var wg sync.WaitGroup
		for i, a := range run {
			if !approved[i] {
				continue
			}
			wg.Add(1)
			go func(i int, a protocol.Action) {
				defer wg.Done()
				tr, err := executeTool(port, ctx, a)
				if err != nil { // handler crash ≠ observation; contain it
					tr = protocol.ToolResult{ActionID: a.ID, Kind: string(a.Kind), OK: false, Error: err.Error()}
				}
				// Tool handlers are not allowed to alter the correlation identity
				// or payload namespace established by the action being executed.
				tr.ActionID = a.ID
				tr.Kind = string(a.Kind)
				results[i] = tr
			}(i, a)
		}
		wg.Wait()

		for i := range run {
			a, tr := run[i], results[i]
			if !approved[i] {
				denied := ToolCallDeniedEvent{Step: step, Action: a, Tool: c.toolSpec(a.Kind), Result: tr, Decision: *decisions[i]}
				if err := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *ToolCallDeniedEvent) error {
					return h.OnToolCallDenied
				}, &denied); err != nil {
					return result, fmt.Errorf("tool call denied: %w", err)
				}
				results[i] = denied.Result
				results[i].ActionID, results[i].Kind, results[i].OK = a.ID, string(a.Kind), false
				if err := c.emit(Event{Type: EventActionDenied, Step: step, Action: &a,
					Result: &results[i], Tool: c.toolSpec(a.Kind), Decision: decisions[i]}); err != nil {
					return result, fmt.Errorf("event action_denied: %w", err)
				}
				continue
			}
			if !tr.OK {
				failure := ToolCallErrorEvent{Step: step, Action: a, Tool: c.toolSpec(a.Kind), Result: tr, Err: fmt.Errorf("tool call failed: %s", tr.Error)}
				if err := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *ToolCallErrorEvent) error {
					return h.OnToolCallError
				}, &failure); err != nil {
					return result, fmt.Errorf("tool call error: %w", err)
				}
				tr = failure.Result
			}
			completed := ToolCallEndEvent{Step: step, Action: a, Tool: c.toolSpec(a.Kind), Result: tr}
			if err := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *ToolCallEndEvent) error {
				return h.OnToolCallEnd
			}, &completed); err != nil {
				return result, fmt.Errorf("tool call end: %w", err)
			}
			results[i] = completed.Result
			results[i].ActionID, results[i].Kind = a.ID, string(a.Kind)
			if err := c.emit(Event{Type: EventActionEnd, Step: step, Action: &a, Result: &results[i], Tool: c.toolSpec(a.Kind)}); err != nil {
				return result, fmt.Errorf("event action_end: %w", err)
			}
			c.logf("[step %d] ← %s ok=%v err=%q", step, a.Kind, tr.OK, tr.Error)
		}

		stepLog := protocol.StepLog{Step: step, Actions: actions, Results: results}
		for i, a := range run {
			recentContext = append(recentContext,
				ActionContextItem{Source: ContextAction, ActionID: a.ID, Kind: a.Kind, Text: string(a.Args)},
				ActionContextItem{Source: ContextToolResult, ActionID: a.ID, Kind: a.Kind, Text: results[i].Observation()},
			)
		}
		recentContext = boundedActionContext(recentContext)
		result.Steps, result.History = step, append(result.History, stepLog)
		for _, h := range c.onSteps {
			if err := h(obs, stepLog); err != nil {
				return result, fmt.Errorf("step hook (step %d): %w", step, err)
			}
		}
		if err := c.emit(Event{Type: EventStepEnd, Step: step, Actions: actions, Results: results}); err != nil {
			return result, fmt.Errorf("event step_end: %w", err)
		}
		stepOpen = false
		stepEnd := AgentStepEndEvent{Step: step, Run: result}
		if err := dispatchHook(ctx, c.hooks, func(h Hooks) func(context.Context, *AgentStepEndEvent) error {
			return h.OnAgentStepEnd
		}, &stepEnd); err != nil {
			return result, fmt.Errorf("agent step end: %w", err)
		}

		if finished {
			result.Answer = answer
			endReason = AgentEndFinished
			if err := c.emit(Event{Type: EventFinish, Step: step, Text: answer}); err != nil {
				return result, fmt.Errorf("event finish: %w", err)
			}
			return result, nil
		}

		var stopReason string
		stopped := false
		for _, tr := range results {
			interp, err := brain.Interpret(ctx, obs, tr)
			if err != nil {
				return result, fmt.Errorf("brain interpret (step %d): %w", step, err)
			}
			if !interp.Continue && !stopped {
				stopped = true
				stopReason = interp.StopReason
			}
		}

		obs.History = append(obs.History, stepLog)
		if stopped {
			endReason = AgentEndStopped
			agentStopReason = stopReason
			c.logf("[step %d] brain stopped: reason_chars=%d", step, len(stopReason))
			if err := c.emit(Event{Type: EventStopped, Step: step, Text: stopReason}); err != nil {
				return result, fmt.Errorf("event stopped: %w", err)
			}
			return result, nil
		}
	}
	result.Exhausted = true
	endReason = AgentEndExhausted
	if err := c.emit(Event{Type: EventExhausted, Step: maxSteps, Text: fmt.Sprintf("exhausted %d steps", maxSteps)}); err != nil {
		return result, fmt.Errorf("event exhausted: %w", err)
	}
	return result, nil
}

func normalizeAssistantParts(parts []AssistantPart, actions []protocol.Action) ([]AssistantPart, error) {
	if len(parts) == 0 {
		parts = make([]AssistantPart, 0, len(actions))
		for _, action := range actions {
			parts = append(parts, AssistantPart{Type: AssistantPartToolCall, Action: action})
		}
		return parts, nil
	}

	remaining := make(map[string]protocol.Action, len(actions))
	for _, action := range actions {
		if action.ID == "" {
			return nil, errors.New("tool action is missing its correlation ID")
		}
		if _, exists := remaining[action.ID]; exists {
			return nil, fmt.Errorf("duplicate tool action ID %q", action.ID)
		}
		remaining[action.ID] = action
	}
	out := make([]AssistantPart, len(parts))
	copy(out, parts)
	for i, part := range out {
		switch part.Type {
		case AssistantPartText:
			if part.Text == "" {
				return nil, fmt.Errorf("assistant part %d has empty text", i)
			}
		case AssistantPartToolCall:
			action, ok := remaining[part.Action.ID]
			if !ok || action.Kind != part.Action.Kind || !slices.Equal(action.Args, part.Action.Args) {
				return nil, fmt.Errorf("assistant part %d does not match an executable action", i)
			}
			delete(remaining, part.Action.ID)
		default:
			return nil, fmt.Errorf("assistant part %d has unsupported type %q", i, part.Type)
		}
	}

	if len(remaining) != 0 {
		return nil, errors.New("assistant response omits an executable action")
	}
	return out, nil
}

func (c *Core) toolSpec(kind protocol.ActionKind) *ToolSpec {
	spec, ok := c.specs[kind]
	if !ok {
		return nil
	}
	spec.Params = append([]ToolParam(nil), spec.Params...)
	spec.InputSchema = append(json.RawMessage(nil), spec.InputSchema...)
	return &spec
}

// buildToolPort composes the dispatch handler with registered middleware
// (outermost wrap = first registered, so middleware order reads top-down).
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
		// Result identity belongs to the harness, not the handler. This keeps
		// in-process tools subject to the same correlation and payload
		// namespace guarantees as external tools.
		tr.ActionID = a.ID
		tr.Kind = string(a.Kind)
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
