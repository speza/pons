package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/protocol"
)

// Plugin returns the plugin for one run: the tools of the servers the run's
// agent is granted, narrowed by each grant's filter. ctx bounds the
// connections Setup opens. A nil logger discards diagnostics.
func (m *Manager) Plugin(ctx context.Context, grants map[string]Filter, logger *slog.Logger) pons.Plugin {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &runPlugin{manager: m, ctx: ctx, grants: grants, logger: logger}
}

type runPlugin struct {
	manager *Manager
	ctx     context.Context
	grants  map[string]Filter
	logger  *slog.Logger
}

// registeredTool is what the approval hook needs to know about a tool.
type registeredTool struct {
	approvalMode string
	readOnly     bool
}

func (p *runPlugin) Setup(core *pons.Core) error {
	names := slices.Sorted(maps.Keys(p.grants))
	var granted []*server
	for _, name := range names {
		if !p.manager.Configured(name) {
			return fmt.Errorf("mcp: granted server %q is not configured", name)
		}
		if entry := p.manager.servers[name]; entry.connect != nil {
			granted = append(granted, entry)
		}
	}

	// Servers are independent, so one slow server costs the run only its own
	// startup deadline.
	listed := make([][]tool, len(granted))
	failures := make([]error, len(granted))
	var wg sync.WaitGroup
	for i, entry := range granted {
		wg.Go(func() { listed[i], failures[i] = entry.listTools(p.ctx) })
	}
	wg.Wait()
	if err := p.ctx.Err(); err != nil {
		return err
	}

	registered := make(map[protocol.ActionKind]registeredTool)
	// The owner is told why a server is unavailable. The brain is told only
	// that it is: the reason can be text the server chose, and host context
	// is not labeled as untrusted the way a tool result is.
	var reasons, missing []string
	unavailable := func(entry *server, reason string) {
		reasons = append(reasons, reason)
		missing = append(missing, fmt.Sprintf("MCP server %q is unavailable in this run, so its tools are missing.", entry.name))
	}
	for i, entry := range granted {
		if failures[i] != nil {
			unavailable(entry, entry.failure(p.ctx, failures[i]).Error())
			continue
		}
		selected, infos := entry.selectTools(listed[i], p.grants[entry.name])
		for _, info := range infos {
			if strings.HasPrefix(info.Status, "skipped") {
				p.logger.Debug("mcp tool skipped", "server", entry.name, "status", info.Status)
			}
		}
		if len(selected) > MaxToolsPerServer {
			unavailable(entry, fmt.Sprintf(
				"MCP server %q offers %d tools, more than the limit of %d; narrow enabledTools",
				entry.name, len(selected), MaxToolsPerServer))
			continue
		}

		for _, selectedTool := range selected {
			if err := core.AddTool(protocol.ActionKind(selectedTool.Kind), pons.ToolDef{
				Description: selectedTool.Description,
				InputSchema: selectedTool.InputSchema,
				Source:      pons.ToolSource{PluginName: "mcp", External: true},
				Handler:     p.handler(entry, selectedTool.Name),
			}); err != nil {
				return fmt.Errorf("mcp server %q: %w", entry.name, err)
			}
			registered[protocol.ActionKind(selectedTool.Kind)] = registeredTool{
				approvalMode: entry.config.approvalMode(),
				readOnly:     selectedTool.ReadOnly,
			}
		}
		p.logger.Debug("mcp server ready", "server", entry.name, "tools", len(selected))
	}

	hooks := pons.Hooks{OnToolCallStart: approvalHook(registered)}
	if len(reasons) > 0 {
		notice := strings.Join(reasons, "\n")
		p.logger.Warn("mcp servers unavailable", "detail", notice)
		hooks.OnAgentStart = func(context.Context, pons.AgentStartInput) (pons.AgentStartOutput, error) {
			return pons.AgentStartOutput{
				SystemMessage:     notice,
				AdditionalContext: strings.Join(missing, "\n"),
			}, nil
		}
	}
	return core.AddHooks(hooks)
}

// payload names where an MCP tool result came from.
type payload struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
}

func (p *runPlugin) handler(entry *server, toolName string) pons.ToolHandler {
	return func(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
		result, err := entry.callTool(ctx, toolName, action.Args)
		if err != nil {
			return protocol.ToolResult{Error: entry.failure(ctx, err).Error()}, nil
		}

		// A server sends structured content alongside the same data as text,
		// so the result carries one form: the text, or the JSON without it.
		text := entry.redact.text(result.Text)
		if text == "" {
			// The redactor knows each secret's JSON-escaped forms too.
			if structured := entry.redact.text(string(result.Structured)); json.Valid([]byte(structured)) {
				text = structured
			}
		}
		encoded, err := json.Marshal(payload{Server: entry.name, Tool: toolName})
		if err != nil {
			return protocol.ToolResult{}, err
		}
		if limit := p.manager.maxResultBytes; len(text)+len(encoded) > limit {
			return protocol.ToolResult{
				Error: fmt.Sprintf("MCP server %q: result exceeds %d bytes", entry.name, limit),
			}, nil
		}

		if result.IsError {
			if text == "" {
				text = "tool reported an error"
			}
			return protocol.ToolResult{Error: text, Payload: encoded}, nil
		}
		return protocol.ToolResult{OK: true, Output: text, Payload: encoded}, nil
	}
}

// approvalHook applies each server's approval mode to its own tools. Other
// tools get no answer. Read-only annotations come from the server, so the
// writes mode is a convenience for a trusted server, not a boundary.
func approvalHook(tools map[protocol.ActionKind]registeredTool) func(context.Context, pons.ToolCallStartInput) (pons.ToolCallStartOutput, error) {
	return func(_ context.Context, in pons.ToolCallStartInput) (pons.ToolCallStartOutput, error) {
		registered, ok := tools[in.Action.Kind]
		switch {
		case !ok, registered.approvalMode == ApprovalAllow:
			return pons.ToolCallStartOutput{}, nil
		case registered.approvalMode == ApprovalWrites && registered.readOnly:
			return pons.ToolCallStartOutput{}, nil
		case registered.approvalMode == ApprovalWrites:
			return pons.ToolCallStartOutput{Permission: pons.PermissionAsk, Reason: "mcp_write_approval"}, nil
		default:
			return pons.ToolCallStartOutput{Permission: pons.PermissionAsk, Reason: "mcp_approval"}, nil
		}
	}
}
