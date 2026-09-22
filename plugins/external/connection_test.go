package external_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/samperrin/pons/plugins/external"
	"github.com/samperrin/pons/plugins/external/sdk"
)

func TestConnectionHostRejectsShutdownTransportFailure(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	errR, errW := io.Pipe()
	stdin := &closingWriter{PipeWriter: inW, closed: make(chan struct{})}
	done := make(chan error, 1)
	streamErr := errors.New("stream reset before process end")
	var kills atomic.Int32
	go func() {
		serveErr := (sdk.Server{Name: "test.connection", Version: "test"}).Serve(context.Background(), inR, outW)
		// Ensure the shutdown response reached the caller before losing transport.
		<-stdin.closed
		_ = outW.Close()
		_ = errW.Close()
		done <- errors.Join(serveErr, streamErr)
	}()
	host, err := external.NewConnectionHost(external.Manifest{
		ManifestVersion: external.ManifestVersion,
		Name:            "test.connection", Entrypoint: "/remote/provider", RuntimeProtocol: external.RuntimeProtocol,
	}, external.HostConfig{Workspace: "/workspace"}, func(context.Context) (external.Connection, error) {
		return external.Connection{
			Stdin: stdin, Stdout: outR, Stderr: errR,
			Wait: func() error { return <-done },
			Kill: func() error { kills.Add(1); return nil },
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := host.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := host.Close(); !errors.Is(err, streamErr) {
		t.Fatalf("Close = %v", err)
	}
	if kills.Load() != 1 {
		t.Fatalf("Kill calls = %d", kills.Load())
	}
}

type closingWriter struct {
	*io.PipeWriter
	closed chan struct{}
	once   sync.Once
}

func (w *closingWriter) Close() error {
	w.once.Do(func() { close(w.closed) })
	return w.PipeWriter.Close()
}

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
