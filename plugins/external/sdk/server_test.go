package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/samperrin/pons/protocol"
)

func TestServeContextCancellationInterruptsIdleRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, reader, io.Discard, Server{Name: "test.plugin"})
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Serve error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve did not interrupt an idle protocol read")
	}
}

type failAfterWriter struct{ writes int }

func (w *failAfterWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("broken plugin stdout")
	}
	return len(p), nil
}

func TestAsyncResponseWriteFailureStopsServer(t *testing.T) {
	const schema = `{"type":"object","properties":{},"additionalProperties":false}`
	reader, writer := io.Pipe()
	defer writer.Close()
	output := &failAfterWriter{}
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), reader, output, Server{
			Name: "test.plugin",
			Tools: []Tool{{Kind: "echo", Description: "echo", InputSchema: json.RawMessage(schema), Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
				return protocol.ToolResult{OK: true, Output: "hello"}, nil
			}}},
		})
	}()
	_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":"init","method":"plugin/initialize","params":{"runtime_protocol":1,"host":{"name":"pons","version":"test","placement":"hands"},"supported_capabilities":{"tool_provider":[1]}}}`+"\n")
	_, _ = io.WriteString(writer, `{"jsonrpc":"2.0","id":"call","method":"tools/execute","params":{"action":{"id":"action-1","kind":"echo","args":{}}}}`+"\n")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "broken plugin stdout") {
			t.Fatalf("Serve error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("async response write failure did not stop Serve")
	}
}

func TestServerNormalizesHandlerResultIdentity(t *testing.T) {
	const schema = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"init","method":"plugin/initialize","params":{"runtime_protocol":1,"host":{"name":"pons","version":"test","placement":"hands"},"supported_capabilities":{"tool_provider":[1]}}}`,
		`{"jsonrpc":"2.0","id":"call","method":"tools/execute","params":{"action":{"id":"action-1","kind":"echo","args":{"text":"hi"}}}}`,
		`{"jsonrpc":"2.0","id":"shutdown","method":"plugin/shutdown"}`,
	}, "\n") + "\n"
	var output bytes.Buffer
	err := Serve(context.Background(), strings.NewReader(input), &output, Server{
		Name: "test.plugin",
		Tools: []Tool{{
			Kind: "echo", Description: "echo", InputSchema: json.RawMessage(schema),
			Handler: func(context.Context, protocol.Action) (protocol.ToolResult, error) {
				return protocol.ToolResult{ActionID: "forged-id", Kind: "forged-kind", OK: true, Output: "ok"}, nil
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var callResult protocol.ToolResult
	for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
		var response struct {
			ID     string          `json:"id"`
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatal(err)
		}
		if response.ID == "call" {
			if err := json.Unmarshal(response.Result, &callResult); err != nil {
				t.Fatal(err)
			}
		}
	}
	if callResult.ActionID != "action-1" || callResult.Kind != "echo" {
		t.Fatalf("handler identity was not normalized: %+v", callResult)
	}
}
