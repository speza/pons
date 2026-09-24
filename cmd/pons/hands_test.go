package main

import (
	"context"
	"io"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/internal/toolhost"
	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/protocol"
)

// pipeHands serves the real pons-hands tool host over in-memory pipes. It is
// the default suite's stand-in for a sandbox: same tools and protocol, no
// isolation, so tests need neither macOS Seatbelt nor E2B.
type pipeHands struct{}

func testServerOptions(opts serverOptions) serverOptions {
	opts.Sandbox, opts.Environment = "test", pipeHands{}
	return opts
}

func (pipeHands) Start(ctx context.Context, spec environment.Spec) (environment.HandsSession, error) {
	tools, err := toolhost.New(toolhost.Config{Workspace: spec.WorkspacePath, BashTimeout: 10})
	if err != nil {
		return nil, err
	}
	host, err := external.NewConnectionHost(external.Manifest{
		ManifestVersion: external.ManifestVersion,
		Name:            "pons.hands",
		Entrypoint:      "/test/pons-hands",
		RuntimeProtocol: external.RuntimeProtocol,
	}, external.HostConfig{Workspace: spec.WorkspacePath, CallTimeout: 30 * time.Second},
		func(context.Context) (external.Connection, error) {
			inR, inW := io.Pipe()
			outR, outW := io.Pipe()
			errR, errW := io.Pipe()
			serveCtx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				err := tools.Serve(serveCtx, inR, outW)
				_ = outW.Close()
				_ = errW.Close()
				done <- err
			}()
			var once sync.Once
			return external.Connection{
				Stdin: inW, Stdout: outR, Stderr: errR,
				Wait: func() error { return <-done },
				Kill: func() error {
					once.Do(func() {
						cancel()
						_ = inR.Close()
						_ = outR.Close()
						_ = errR.Close()
					})
					return nil
				},
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if err := host.Start(ctx); err != nil {
		return nil, err
	}
	return &pipeSession{host: host, tools: tools, metadata: environment.Metadata{
		Provider: "test", WorkspaceID: spec.WorkspaceID, WorkspacePath: spec.WorkspacePath,
		ReadWrite: spec.ReadWrite, Platform: goruntime.GOOS + "/" + goruntime.GOARCH,
		Network: environment.NetworkDisabled,
	}}, nil
}

type pipeSession struct {
	host     *external.Host
	tools    *toolhost.Host
	metadata environment.Metadata
}

func (s *pipeSession) Catalog() []external.ToolDescription { return s.host.Tools() }
func (s *pipeSession) Metadata() environment.Metadata      { return s.metadata }
func (s *pipeSession) Execute(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
	return s.host.Execute(ctx, action)
}
func (s *pipeSession) Close() error {
	hostErr := s.host.Close()
	if err := s.tools.Close(); hostErr == nil {
		hostErr = err
	}
	return hostErr
}
