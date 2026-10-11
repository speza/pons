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
	var unavailable []string
	for i, entry := range granted {
		if failures[i] != nil {
			unavailable = append(unavailable, entry.failure(p.ctx, failures[i]).Error())
			continue
		}
		selected := p.selectTools(entry, listed[i])
		if len(selected) > maxToolsPerServer {
			unavailable = append(unavailable, fmt.Sprintf(
				"MCP server %q offers %d tools, more than the limit of %d; narrow enabledTools",
				entry.name, len(selected), maxToolsPerServer))
			continue
		}

		for _, selectedTool := range selected {
			if err := core.AddTool(protocol.ActionKind(selectedTool.Kind), pons.ToolDef{
				Description: selectedTool.Description,
				InputSchema: selectedTool.InputSchema,
				Source:      pons.ToolSource{PluginName: "mcp", External: true, Server: entry.name},
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
	if len(unavailable) > 0 {
		notice := strings.Join(unavailable, "\n")
		p.logger.Warn("mcp servers unavailable", "detail", notice)
		hooks.OnAgentStart = func(context.Context, pons.AgentStartInput) (pons.AgentStartOutput, error) {
			return pons.AgentStartOutput{
				SystemMessage:     notice,
				AdditionalContext: "These tools are unavailable in this run. " + notice,
			}, nil
		}
	}
	return core.AddHooks(hooks)
}

// selectTools applies the host filter, the grant filter, and the limits on
// what a server may report, keeping the server's order.
func (p *runPlugin) selectTools(entry *server, tools []tool) []preparedTool {
	host, grant := entry.config.filter(), p.grants[entry.name]
	seen := make(map[string]bool, len(tools))
	var selected []preparedTool
	for _, reported := range tools {
		if !host.allows(reported.Name) || !grant.allows(reported.Name) {
			continue
		}
		prepared, reason := entry.prepare(reported)
		if reason == "" && seen[reported.Name] {
			reason = "tool is listed twice"
		}
		if reason != "" {
			p.logger.Debug("mcp tool skipped", "server", entry.name, "reason", reason)
			continue
		}
		seen[reported.Name] = true
		selected = append(selected, prepared)
	}
	return selected
}

// payload is the structured result of an MCP tool call.
type payload struct {
	Server     string          `json:"server"`
	Tool       string          `json:"tool"`
	Structured json.RawMessage `json:"structured,omitempty"`
}

func (p *runPlugin) handler(entry *server, toolName string) pons.ToolHandler {
	return func(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
		result, err := entry.callTool(ctx, toolName, action.Args)
		if err != nil {
			return protocol.ToolResult{Error: entry.failure(ctx, err).Error()}, nil
		}

		text := entry.redact.text(result.Text)
		structured := json.RawMessage(entry.redact.text(string(result.Structured)))
		if !json.Valid(structured) {
			structured = nil
		}
		if text == "" {
			text = string(structured)
		}
		encoded, err := json.Marshal(payload{Server: entry.name, Tool: toolName, Structured: structured})
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
