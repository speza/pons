package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/plugins/external"
)

const (
	maxE2BJSONResponseBytes  = 1 << 20
	maxE2BProcessOutputBytes = 1 << 20
)

type e2bClient struct {
	apiKey, apiURL, envdURL string
	http                    *http.Client
}

type e2bSandbox struct {
	ID          string `json:"sandboxID"`
	AccessToken string `json:"envdAccessToken"`
}

func (c *e2bClient) connectSandbox(ctx context.Context, id string, timeout time.Duration) (e2bSandbox, error) {
	payload := struct {
		Timeout int `json:"timeout"`
	}{Timeout: max(1, int(timeout.Seconds()))}
	var sandbox e2bSandbox
	err := c.jsonRequest(
		ctx,
		http.MethodPost,
		c.apiURL+"/sandboxes/"+url.PathEscape(id)+"/connect",
		payload,
		&sandbox,
		false,
		e2bSandbox{},
		http.StatusOK,
		http.StatusCreated, // A paused sandbox returns 201 when resumed.
	)
	if err != nil {
		return sandbox, err
	}
	if sandbox.ID == "" || sandbox.AccessToken == "" {
		return sandbox, errors.New("environment: E2B connect response omitted sandbox credentials")
	}
	return sandbox, nil
}

func (c *e2bClient) createSandbox(
	ctx context.Context,
	template string,
	timeout time.Duration,
	network environment.NetworkPolicy,
	env map[string]string,
) (e2bSandbox, error) {
	payload := struct {
		Template string            `json:"templateID"`
		Timeout  int               `json:"timeout"`
		Secure   bool              `json:"secure"`
		Internet bool              `json:"allow_internet_access"`
		Env      map[string]string `json:"envVars,omitempty"`
	}{
		Template: template,
		Timeout:  max(1, int(timeout.Seconds())),
		Secure:   true,
		Internet: network == environment.NetworkEnabled,
		Env:      env,
	}
	var sandbox e2bSandbox
	if err := c.jsonRequest(ctx, http.MethodPost, c.apiURL+"/sandboxes", payload, &sandbox, false, e2bSandbox{}, http.StatusCreated); err != nil {
		return sandbox, fmt.Errorf("environment: create E2B sandbox: %w", err)
	}
	if sandbox.ID == "" || sandbox.AccessToken == "" {
		err := errors.New("environment: E2B create response omitted sandbox credentials")
		if sandbox.ID != "" {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err = errors.Join(err, c.killSandbox(cleanupCtx, sandbox.ID))
		}
		return sandbox, err
	}
	return sandbox, nil
}

func (c *e2bClient) setSandboxTimeout(ctx context.Context, id string, timeout time.Duration) error {
	payload := struct {
		Timeout int `json:"timeout"`
	}{Timeout: max(1, int(timeout.Seconds()))}
	if err := c.jsonRequest(
		ctx,
		http.MethodPost,
		c.apiURL+"/sandboxes/"+url.PathEscape(id)+"/timeout",
		payload,
		nil,
		false,
		e2bSandbox{},
		http.StatusNoContent,
	); err != nil {
		return fmt.Errorf("environment: set E2B sandbox timeout: %w", err)
	}
	return nil
}

func (c *e2bClient) killSandbox(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.apiURL+"/sandboxes/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", c.apiKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("environment: kill E2B sandbox: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		return responseError("kill E2B sandbox", resp)
	}
	return nil
}

func (c *e2bClient) upload(ctx context.Context, sandbox e2bSandbox, path string, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.envdURL+"/files?path="+url.QueryEscape(path), body)
	if err != nil {
		return err
	}
	c.envdHeaders(req, sandbox)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("environment: upload E2B workspace: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return responseError("upload E2B workspace", resp)
	}
	return nil
}

func (c *e2bClient) download(ctx context.Context, sandbox e2bSandbox, path string, limit int64) ([]byte, error) {
	if limit < 0 || limit == math.MaxInt64 {
		return nil, errors.New("environment: invalid E2B download limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.envdURL+"/files?path="+url.QueryEscape(path), nil)
	if err != nil {
		return nil, err
	}
	c.envdHeaders(req, sandbox)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("environment: download E2B workspace: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, responseError("download E2B workspace", resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("environment: E2B workspace archive exceeds %d bytes", limit)
	}
	return body, nil
}

func (c *e2bClient) envdHeaders(req *http.Request, sandbox e2bSandbox) {
	req.Header.Set("E2b-Sandbox-Id", sandbox.ID)
	req.Header.Set("E2b-Sandbox-Port", "49983")
	req.Header.Set("X-Access-Token", sandbox.AccessToken)
}

func (c *e2bClient) run(ctx context.Context, sandbox e2bSandbox, command string, args []string, cwd string, env map[string]string) ([]byte, []byte, error) {
	stream, err := c.startProcess(ctx, sandbox, command, args, cwd, env, false, "")
	if err != nil {
		return nil, nil, err
	}
	defer stream.body.Close()
	// Maintenance commands are sandbox-controlled too. Bound total output,
	// not just individual envd frames, before retaining it on the host.
	stdout := boundedProcessBuffer{remaining: maxE2BProcessOutputBytes}
	stderr := boundedProcessBuffer{remaining: maxE2BProcessOutputBytes}
	for {
		event, err := stream.next()
		if err != nil {
			return stdout.Bytes(), stderr.Bytes(), err
		}
		if err := writeProcessData(event.Event.Data, &stdout, &stderr); err != nil {
			return nil, nil, fmt.Errorf("environment: E2B control process output: %w", err)
		}
		if event.Event.End != nil {
			if err := e2bProcessEndError(event.Event.End, stderr.String()); err != nil {
				return stdout.Bytes(), stderr.Bytes(), err
			}
			return stdout.Bytes(), stderr.Bytes(), nil
		}
	}
}

func (c *e2bClient) startConnection(ctx context.Context, sandbox e2bSandbox, command string, args []string, cwd string, env map[string]string) (external.Connection, error) {
	stream, err := c.startProcess(ctx, sandbox, command, args, cwd, env, true, "pons-hands")
	if err != nil {
		return external.Connection{}, err
	}
	var pid int
	for pid == 0 {
		event, nextErr := stream.next()
		if nextErr != nil {
			stream.body.Close()
			return external.Connection{}, nextErr
		}
		if event.Event.Start != nil {
			pid = event.Event.Start.PID
		}
	}
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()
	var streamCloseOnce sync.Once
	closeStream := func() {
		streamCloseOnce.Do(func() { _ = stream.body.Close() })
	}
	done := make(chan error, 1)
	go func() {
		defer closeStream()
		defer stdoutW.Close()
		defer stderrW.Close()
		for {
			event, nextErr := stream.next()
			if nextErr != nil {
				done <- nextErr
				return
			}
			if err := writeProcessData(event.Event.Data, stdoutW, stderrW); err != nil {
				done <- err
				return
			}
			if event.Event.End != nil {
				done <- e2bProcessEndError(event.Event.End, "")
				return
			}
		}
	}()
	stdin := &e2bProcessWriter{client: c, sandbox: sandbox, pid: pid}
	var killOnce sync.Once
	var killErr error
	return external.Connection{
		Stdin: stdin, Stdout: stdoutR, Stderr: stderrR,
		Wait: func() error { return <-done },
		Kill: func() error {
			killOnce.Do(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				killErr = c.signal(ctx, sandbox, pid, "SIGNAL_SIGKILL")
				cancel()
				// Unblock the local stream pump even if envd never reports the
				// process end after accepting the signal.
				_ = stdoutR.Close()
				_ = stderrR.Close()
				closeStream()
			})
			return killErr
		},
	}, nil
}

type e2bProcessWriter struct {
	client  *e2bClient
	sandbox e2bSandbox
	pid     int
	writeMu sync.Mutex
	mu      sync.Mutex
	closed  bool
	cancel  context.CancelFunc
}

func (w *e2bProcessWriter) Write(data []byte) (int, error) {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return 0, os.ErrClosed
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	w.cancel = cancel
	w.mu.Unlock()
	defer func() {
		cancel()
		w.mu.Lock()
		w.cancel = nil
		w.mu.Unlock()
	}()
	payload := map[string]any{
		"process": map[string]int{"pid": w.pid},
		"input": map[string]string{
			"stdin": base64.StdEncoding.EncodeToString(data),
		},
	}
	if err := w.client.unary(ctx, w.sandbox, "/process.Process/SendInput", payload); err != nil {
		return 0, err
	}
	return len(data), nil
}
func (w *e2bProcessWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	if w.cancel != nil {
		w.cancel()
	}
	// Closing the local writer must never wait on envd. The protocol shutdown
	// exchange stops hands normally; Connection.Kill handles failed shutdown.
	return nil
}

func (c *e2bClient) signal(ctx context.Context, sandbox e2bSandbox, pid int, signal string) error {
	return c.unary(ctx, sandbox, "/process.Process/SendSignal", map[string]any{
		"process": map[string]int{"pid": pid},
		"signal":  signal,
	})
}

func (c *e2bClient) unary(ctx context.Context, sandbox e2bSandbox, path string, payload any) error {
	return c.jsonRequest(ctx, http.MethodPost, c.envdURL+path, payload, nil, true, sandbox, http.StatusOK)
}

func (c *e2bClient) jsonRequest(ctx context.Context, method, target string, payload, result any, envd bool, sandbox e2bSandbox, statuses ...int) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if envd {
		c.envdHeaders(req, sandbox)
		req.Header.Set("Connect-Protocol-Version", "1")
	} else {
		req.Header.Set("X-API-Key", c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if !slices.Contains(statuses, resp.StatusCode) {
		return responseError("E2B request", resp)
	}
	if result != nil {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxE2BJSONResponseBytes+1))
		if err != nil {
			return err
		}
		if len(body) > maxE2BJSONResponseBytes {
			return fmt.Errorf("environment: E2B JSON response exceeds %d bytes", maxE2BJSONResponseBytes)
		}
		return json.Unmarshal(body, result)
	}
	return nil
}

type e2bProcessStream struct{ body io.ReadCloser }

type e2bProcessEnd struct {
	Status string  `json:"status"`
	Error  *string `json:"error"`
}

func e2bProcessEndError(end *e2bProcessEnd, stderr string) error {
	if end.Error != nil {
		message := strings.TrimSpace(*end.Error)
		if message == "" {
			message = "unknown process error"
		}
		return fmt.Errorf("E2B process failed: %s", message)
	}
	if end.Status != "" && !strings.HasSuffix(end.Status, " 0") {
		if stderr = strings.TrimSpace(stderr); stderr != "" {
			return fmt.Errorf("%s: %s", end.Status, stderr)
		}
		return errors.New(end.Status)
	}
	return nil
}

type e2bProcessData struct {
	Stdout string `json:"stdout,omitempty"`
	Stderr string `json:"stderr,omitempty"`
}

type boundedProcessBuffer struct {
	bytes.Buffer
	remaining int
}

func (b *boundedProcessBuffer) Write(data []byte) (int, error) {
	if len(data) > b.remaining {
		return 0, fmt.Errorf("environment: E2B process output exceeds %d bytes", maxE2BProcessOutputBytes)
	}
	n, err := b.Buffer.Write(data)
	b.remaining -= n
	return n, err
}

func writeProcessData(data *e2bProcessData, stdout, stderr io.Writer) error {
	if data == nil {
		return nil
	}
	if err := writeEncodedProcessData(data.Stdout, stdout); err != nil {
		return err
	}
	return writeEncodedProcessData(data.Stderr, stderr)
}

func writeEncodedProcessData(encoded string, writer io.Writer) error {
	if encoded == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	_, err = writer.Write(decoded)
	return err
}

type e2bProcessResponse struct {
	Event struct {
		Start *struct {
			PID int `json:"pid"`
		} `json:"start,omitempty"`
		Data *e2bProcessData `json:"data,omitempty"`
		End  *e2bProcessEnd  `json:"end,omitempty"`
	} `json:"event"`
}

func (c *e2bClient) startProcess(
	ctx context.Context,
	sandbox e2bSandbox,
	command string,
	args []string,
	cwd string,
	env map[string]string,
	stdin bool,
	tag string,
) (*e2bProcessStream, error) {
	payload := map[string]any{
		"process": map[string]any{
			"cmd":  command,
			"args": args,
			"cwd":  cwd,
			"envs": env,
		},
		"stdin": stdin,
	}
	if tag != "" {
		payload["tag"] = tag
	}
	message, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var framed bytes.Buffer
	framed.WriteByte(0)
	_ = binary.Write(&framed, binary.BigEndian, uint32(len(message)))
	framed.Write(message)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.envdURL+"/process.Process/Start", &framed)
	if err != nil {
		return nil, err
	}
	c.envdHeaders(req, sandbox)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Content-Type", "application/connect+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, responseError("start E2B process", resp)
	}
	return &e2bProcessStream{body: resp.Body}, nil
}

func (s *e2bProcessStream) next() (e2bProcessResponse, error) {
	var response e2bProcessResponse
	header := make([]byte, 5)
	if _, err := io.ReadFull(s.body, header); err != nil {
		return response, err
	}
	length := binary.BigEndian.Uint32(header[1:])
	if length > 16<<20 {
		return response, fmt.Errorf("environment: E2B process frame exceeds limit: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(s.body, body); err != nil {
		return response, err
	}
	if header[0]&2 != 0 {
		var end struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &end); err != nil {
			return response, err
		}
		if end.Error != nil {
			return response, errors.New(end.Error.Message)
		}
		return response, io.EOF
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return response, err
	}
	return response, nil
}

type httpStatusError struct {
	Operation  string
	Status     int
	StatusText string
	Body       []byte
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("environment: %s: %s: %s", e.Operation, e.StatusText, strings.TrimSpace(string(e.Body)))
}

func hasHTTPStatus(err error, status int) bool {
	statusErr := new(httpStatusError)
	return errors.As(err, &statusErr) && statusErr.Status == status
}

func responseError(operation string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return &httpStatusError{Operation: operation, Status: resp.StatusCode, StatusText: resp.Status, Body: body}
}
