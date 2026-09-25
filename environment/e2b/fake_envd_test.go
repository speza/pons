package e2b

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/samperrin/pons/plugins/external/sdk"
)

// Shared pieces of the fake envd API used by E2B tests.

type startRequest struct {
	Process struct {
		Cmd  string
		Args []string
	}
}

// decodeStart decodes a Connect-framed Process.Start request body.
func decodeStart(body []byte) (startRequest, bool) {
	var request startRequest
	if len(body) < 5 || json.Unmarshal(body[5:], &request) != nil {
		return request, false
	}
	return request, true
}

// writeEnd reports a successful process exit.
func writeEnd(w io.Writer) {
	_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"end": map[string]string{"status": "exit status 0"}}})
}

// forwardInput writes a SendInput request's stdin to the running hands.
func forwardInput(r *http.Request, input *atomic.Pointer[io.PipeWriter]) error {
	var payload struct{ Input struct{ Stdin string } }
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return err
	}
	body, err := base64.StdEncoding.DecodeString(payload.Input.Stdin)
	if err == nil {
		_, err = input.Load().Write(body)
	}
	return err
}

// serveHands answers a hands Process.Start with an in-process tool server,
// streaming its stdout as Connect frames until it exits.
func serveHands(w http.ResponseWriter, r *http.Request, input *atomic.Pointer[io.PipeWriter]) {
	w.Header().Set("Content-Type", "application/connect+json")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	input.Store(inW)
	defer inW.Close()
	defer outR.Close()
	go func() {
		serveErr := (sdk.Server{Name: "pons.hands", Version: "test"}).Serve(r.Context(), inR, outW)
		_ = outW.CloseWithError(serveErr)
	}()
	_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"start": map[string]int{"pid": 42}}})
	w.(http.Flusher).Flush()
	buffer := make([]byte, 4096)
	for {
		n, err := outR.Read(buffer)
		if n > 0 {
			_ = writeConnectFrame(w, map[string]any{"event": map[string]any{"data": map[string]string{"stdout": base64.StdEncoding.EncodeToString(buffer[:n])}}})
			w.(http.Flusher).Flush()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				writeEnd(w)
			}
			return
		}
	}
}
