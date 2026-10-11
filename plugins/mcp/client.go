package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxListedTools bounds how many tools one server may report before filters.
const maxListedTools = 1024

// errSessionGone reports that the server no longer knows the session. Every
// call in flight on the session fails with it, including ones the server had
// already accepted.
var errSessionGone = errors.New("mcp: server ended the session")

// errSessionBroken reports a session whose connection can no longer be used.
var errSessionBroken = errors.New("mcp: connection lost")

// tool is one entry of a server's tool list, as the server reported it.
type tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	ReadOnly    bool
}

// callResult is a tools/call result reduced to what pons uses.
type callResult struct {
	Text       string
	Structured json.RawMessage
	IsError    bool
}

// session is one open connection to a server. It is the only seam to the MCP
// SDK, and is safe for concurrent calls.
type session interface {
	ListTools(ctx context.Context) ([]tool, error)
	CallTool(ctx context.Context, name string, args json.RawMessage) (callResult, error)
	Close() error
}

// connector opens a session.
type connector func(ctx context.Context) (session, error)

// sdkConnector opens Streamable HTTP sessions with the official SDK. pons
// advertises no client capabilities: no roots, sampling, or elicitation.
func sdkConnector(endpoint string, client *http.Client) connector {
	return func(ctx context.Context) (session, error) {
		mcpClient := sdk.NewClient(
			&sdk.Implementation{Name: "pons", Version: "1"},
			&sdk.ClientOptions{Capabilities: &sdk.ClientCapabilities{}},
		)
		opened, err := mcpClient.Connect(ctx, &sdk.StreamableClientTransport{
			Endpoint:   endpoint,
			HTTPClient: client,
			// Tools are listed at the start of each run, so no server-initiated
			// stream is needed.
			DisableStandaloneSSE: true,
		}, nil)
		if err != nil {
			return nil, err
		}
		return sdkSession{opened}, nil
	}
}

type sdkSession struct{ session *sdk.ClientSession }

func (s sdkSession) Close() error { return s.session.Close() }

func (s sdkSession) ListTools(ctx context.Context) ([]tool, error) {
	var tools []tool
	params := &sdk.ListToolsParams{}
	for {
		page, err := s.session.ListTools(ctx, params)
		if err != nil {
			return nil, sessionError(err)
		}
		for _, reported := range page.Tools {
			if reported == nil {
				continue
			}
			if len(tools) == maxListedTools {
				return nil, fmt.Errorf("server reports more than %d tools", maxListedTools)
			}
			schema, err := json.Marshal(reported.InputSchema)
			if err != nil {
				schema = nil
			}
			tools = append(tools, tool{
				Name:        reported.Name,
				Description: reported.Description,
				InputSchema: schema,
				ReadOnly:    reported.Annotations != nil && reported.Annotations.ReadOnlyHint,
			})
		}
		if page.NextCursor == "" || page.NextCursor == params.Cursor {
			return tools, nil
		}
		params = &sdk.ListToolsParams{Cursor: page.NextCursor}
	}
}

func (s sdkSession) CallTool(ctx context.Context, name string, args json.RawMessage) (callResult, error) {
	if trimmed := bytes.TrimSpace(args); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		args = json.RawMessage(`{}`)
	}
	result, err := s.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return callResult{}, sessionError(err)
	}
	if len(result.InputRequests) > 0 {
		return callResult{}, errors.New("server asked for more input, which pons does not support")
	}

	var text []string
	for _, content := range result.Content {
		switch content := content.(type) {
		case *sdk.TextContent:
			text = append(text, content.Text)
		case *sdk.ImageContent:
			text = append(text, "[image content omitted]")
		case *sdk.AudioContent:
			text = append(text, "[audio content omitted]")
		case *sdk.ResourceLink:
			text = append(text, "[resource link omitted]")
		case *sdk.EmbeddedResource:
			text = append(text, "[embedded resource omitted]")
		default:
			text = append(text, "[unsupported content omitted]")
		}
	}
	out := callResult{Text: strings.Join(text, "\n"), IsError: result.IsError}
	if result.StructuredContent != nil {
		if out.Structured, err = json.Marshal(result.StructuredContent); err != nil {
			return callResult{}, fmt.Errorf("decode structured content: %w", err)
		}
	}
	return out, nil
}

func sessionError(err error) error {
	switch {
	case errors.Is(err, sdk.ErrSessionMissing):
		return errSessionGone
	case errors.Is(err, sdk.ErrConnectionClosed):
		return fmt.Errorf("%w: %v", errSessionBroken, err)
	default:
		return err
	}
}

// callState is what the transport learned about one tool call's request. It
// travels in the call's context, so a failure that reaches several calls at
// once can still be told apart per call.
type callState struct {
	// sent is set once the request was handed to the network.
	sent atomic.Bool
	// refused is set when the server answered this request by rejecting its
	// session, which means it did not run the tool.
	refused atomic.Bool
}

type callStateKey struct{}

func withCallState(ctx context.Context, state *callState) context.Context {
	return context.WithValue(ctx, callStateKey{}, state)
}

// errUnauthorized is the error for an HTTP 401 from a server.
type errUnauthorized struct{ hasAuthorization bool }

func (e errUnauthorized) Error() string {
	if e.hasAuthorization {
		return "server rejected the configured Authorization header (HTTP 401); check its secret"
	}
	return "server requires authorization (HTTP 401); OAuth login is not supported yet, " +
		"so configure a token in the server's \"headers\""
}

// headerTransport is the only place a server's resolved secrets exist. It
// adds the configured headers to every request and bounds each response.
type headerTransport struct {
	base         http.RoundTripper
	headers      map[string]string
	maxBodyBytes int64
}

func (t headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	request = request.Clone(request.Context())
	for name, value := range t.headers {
		request.Header.Set(name, value)
	}
	state, _ := request.Context().Value(callStateKey{}).(*callState)
	if state != nil {
		state.sent.Store(true)
	}

	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if state != nil && response.StatusCode == http.StatusNotFound && request.Header.Get("Mcp-Session-Id") != "" {
		state.refused.Store(true)
	}
	if response.StatusCode == http.StatusUnauthorized {
		_ = response.Body.Close()
		return nil, errUnauthorized{hasAuthorization: request.Header.Get("Authorization") != ""}
	}
	response.Body = &cappedBody{body: response.Body, remaining: t.maxBodyBytes, limit: t.maxBodyBytes}
	return response, nil
}

// newHTTPClient builds the client for one server. Redirects are refused so a
// server's headers can never follow one to another host.
func newHTTPClient(headers map[string]string, maxBodyBytes int64) *http.Client {
	return &http.Client{
		Transport: headerTransport{
			base:         http.DefaultTransport.(*http.Transport).Clone(),
			headers:      headers,
			maxBodyBytes: maxBodyBytes,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("server redirected the request; redirects are refused")
		},
	}
}

// cappedBody fails a response that grows past the limit instead of
// truncating it into something that might still parse.
type cappedBody struct {
	body      io.ReadCloser
	remaining int64
	limit     int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		// A response of exactly the limit still has to report its end.
		var probe [1]byte
		if n, err := b.body.Read(probe[:]); n == 0 {
			return 0, err
		}
		return 0, fmt.Errorf("server response exceeds %d bytes", b.limit)
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.body.Close() }
