package external_test

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/samperrin/pons/plugins/external"
)

func TestConnectionHostCleansUpIncompleteConnection(t *testing.T) {
	stdin := &trackingStream{}
	stderr := &trackingStream{}
	killed := false
	host, err := external.NewConnectionHost(external.Manifest{
		ManifestVersion: external.ManifestVersion,
		Name:            "test.connection",
		Entrypoint:      "/remote/provider",
		RuntimeProtocol: external.RuntimeProtocol,
	}, external.HostConfig{Workspace: "/workspace"}, func(context.Context) (external.Connection, error) {
		return external.Connection{
			Stdin: stdin, Stderr: stderr,
			Kill: func() error {
				killed = true
				return nil
			},
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "incomplete connection") {
		t.Fatalf("error = %v", err)
	}
	if !killed || !stdin.closed || !stderr.closed {
		t.Fatalf("cleanup: killed=%t stdin=%t stderr=%t", killed, stdin.closed, stderr.closed)
	}
}

type trackingStream struct{ closed bool }

func (*trackingStream) Read([]byte) (int, error)    { return 0, io.EOF }
func (*trackingStream) Write(p []byte) (int, error) { return len(p), nil }
func (s *trackingStream) Close() error {
	s.closed = true
	return nil
}
