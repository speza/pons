// Package toolhost composes and serves a tool catalog behind the hands
// boundary. It deliberately contains no brain, conversation state, or model
// credentials.
package toolhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/bash"
	"github.com/samperrin/pons/plugins/edit"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/plugins/external/sdk"
	"github.com/samperrin/pons/plugins/fs"
)

// Config describes one tool host. Environment placement and filesystem
// policy are intentionally owned by the process launcher, not this package.
type Config struct {
	Workspace string
	// ReadWrite lists directories outside the workspace, such as an agent's
	// memory copy, that file tools may address by absolute path.
	ReadWrite           []string
	FSReadBytes         int
	BashTimeout         int
	BashMaxLines        int
	BashMaxBytes        int
	ExternalManifests   []string
	ExternalPath        string
	ExternalResultBytes int
	ExternalCallTimeout time.Duration
	MaxConcurrency      int
}

// Host owns a composed tool catalog and any nested external providers.
type Host struct {
	externals []*external.Plugin
	server    sdk.Server
}

// New builds and validates the complete hands tool catalog.
func New(cfg Config) (*Host, error) {
	if cfg.Workspace == "" {
		return nil, errors.New("tool host: workspace is required")
	}
	if cfg.MaxConcurrency < 0 {
		return nil, errors.New("tool host: max concurrency must be non-negative")
	}
	fsTools, err := fs.New(fs.Config{Root: cfg.Workspace, ExtraRoots: cfg.ReadWrite, MaxReadBytes: cfg.FSReadBytes})
	if err != nil {
		return nil, err
	}
	editTool, err := edit.New(edit.Config{Root: cfg.Workspace, ExtraRoots: cfg.ReadWrite})
	if err != nil {
		return nil, err
	}
	core := pons.New()
	plugins := []pons.Plugin{
		fsTools,
		editTool,
		bash.New(bash.Config{Root: cfg.Workspace, Timeout: cfg.BashTimeout, MaxLines: cfg.BashMaxLines, MaxBytes: cfg.BashMaxBytes}),
	}
	externals := make([]*external.Plugin, 0, len(cfg.ExternalManifests))
	closeExternals := func() {
		for _, plugin := range slices.Backward(externals) {
			_ = plugin.Close()
		}
	}
	for _, manifest := range cfg.ExternalManifests {
		plugin, err := external.NewHands(manifest, external.HostConfig{
			Workspace:   cfg.Workspace,
			Path:        cfg.ExternalPath,
			CallTimeout: cfg.ExternalCallTimeout,
			Limits:      external.Limits{MaxResultBytes: cfg.ExternalResultBytes},
		})
		if err != nil {
			closeExternals()
			return nil, err
		}
		externals = append(externals, plugin)
		plugins = append(plugins, plugin)
	}
	if err := core.Use(plugins...); err != nil {
		closeExternals()
		return nil, err
	}

	tools := make([]sdk.Tool, 0, len(core.ToolSpecs()))
	for _, spec := range core.ToolSpecs() {
		schema, err := toolSchema(spec)
		if err != nil {
			closeExternals()
			return nil, fmt.Errorf("tool host: tool %q: %w", spec.Kind, err)
		}
		if err := external.ValidateToolSchema(schema); err != nil {
			closeExternals()
			return nil, fmt.Errorf("tool host: tool %q: %w", spec.Kind, err)
		}
		tools = append(tools, sdk.Tool{
			Kind:        string(spec.Kind),
			Description: spec.Description,
			InputSchema: schema,
			Handler:     core.Execute,
		})
	}
	return &Host{
		externals: externals,
		server: sdk.Server{
			Name:           "pons.hands",
			Version:        "0.1.0",
			Tools:          tools,
			MaxConcurrency: cfg.MaxConcurrency,
			Unbounded:      cfg.MaxConcurrency == 0,
		},
	}, nil
}

// Serve runs the tool_provider/v1 endpoint until shutdown or cancellation.
func (h *Host) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	return h.server.Serve(ctx, in, out)
}

// Close releases nested external providers in reverse composition order.
func (h *Host) Close() error {
	var errs []error
	for _, plugin := range slices.Backward(h.externals) {
		if err := plugin.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func toolSchema(spec pons.ToolSpec) (json.RawMessage, error) {
	if len(spec.InputSchema) != 0 {
		return append(json.RawMessage(nil), spec.InputSchema...), nil
	}
	type property struct {
		Type        string `json:"type"`
		Description string `json:"description,omitempty"`
	}
	properties := make(map[string]property, len(spec.Params))
	required := make([]string, 0, len(spec.Params))
	for _, param := range spec.Params {
		if param.Name == "" || param.Type == "" {
			return nil, errors.New("legacy parameter has an empty name or type")
		}
		properties[param.Name] = property{Type: param.Type, Description: param.Description}
		if param.Required {
			required = append(required, param.Name)
		}
	}
	schema := struct {
		Type                 string              `json:"type"`
		Properties           map[string]property `json:"properties"`
		Required             []string            `json:"required,omitempty"`
		AdditionalProperties bool                `json:"additionalProperties"`
	}{
		Type:                 "object",
		Properties:           properties,
		Required:             required,
		AdditionalProperties: false,
	}
	return json.Marshal(schema)
}

var _ pons.ToolPort = (*pons.Core)(nil)
