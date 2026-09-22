package httptransport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	ponsruntime "github.com/samperrin/pons/runtime"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func (c Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Client) endpoint(path string) (string, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		base = "http://127.0.0.1:7337"
	}
	parsed, err := url.Parse(base + path)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("runtime HTTP client: invalid server URL")
	}
	return parsed.String(), nil
}

func (c Client) CreateConversation(ctx context.Context, options ponsruntime.ConversationOptions) (ponsruntime.Conversation, error) {
	endpoint, err := c.endpoint("/v1/conversations")
	if err != nil {
		return ponsruntime.Conversation{}, err
	}
	body, err := json.Marshal(options)
	if err != nil {
		return ponsruntime.Conversation{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ponsruntime.Conversation{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return ponsruntime.Conversation{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return ponsruntime.Conversation{}, responseError(resp)
	}
	var conversation ponsruntime.Conversation
	if err := json.NewDecoder(resp.Body).Decode(&conversation); err != nil {
		return ponsruntime.Conversation{}, err
	}
	return conversation, nil
}

func (c Client) Submit(ctx context.Context, conversationID, key string, parts []ponsruntime.TextPart) (ponsruntime.AcceptedMessage, error) {
	endpoint, err := c.endpoint("/v1/conversations/" + url.PathEscape(conversationID) + "/messages")
	if err != nil {
		return ponsruntime.AcceptedMessage{}, err
	}
	body, err := json.Marshal(messageBody{Parts: parts})
	if err != nil {
		return ponsruntime.AcceptedMessage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return ponsruntime.AcceptedMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return ponsruntime.AcceptedMessage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return ponsruntime.AcceptedMessage{}, responseError(resp)
	}
	var accepted ponsruntime.AcceptedMessage
	if err := json.NewDecoder(resp.Body).Decode(&accepted); err != nil {
		return ponsruntime.AcceptedMessage{}, err
	}
	return accepted, nil
}

func (c Client) View(ctx context.Context, conversationID string) (ponsruntime.ConversationView, error) {
	endpoint, err := c.endpoint("/v1/conversations/" + url.PathEscape(conversationID))
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return ponsruntime.ConversationView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ponsruntime.ConversationView{}, responseError(resp)
	}
	var view ponsruntime.ConversationView
	if err := json.NewDecoder(resp.Body).Decode(&view); err != nil {
		return ponsruntime.ConversationView{}, err
	}
	return view, nil
}

type eventStream struct {
	events <-chan ponsruntime.Event
	errors <-chan error
	close  func()
}

func (c Client) openEvents(ctx context.Context, conversationID string, after uint64) (*eventStream, error) {
	endpoint, err := c.endpoint("/v1/conversations/" + url.PathEscape(conversationID) + "/events?after=" + strconv.FormatUint(after, 10))
	if err != nil {
		return nil, err
	}
	streamCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		cancel()
		return nil, responseError(resp)
	}
	events := make(chan ponsruntime.Event, 32)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		defer resp.Body.Close()
		err := scanSSE(resp.Body, func(event ponsruntime.Event) error {
			select {
			case events <- event:
				return nil
			case <-streamCtx.Done():
				return streamCtx.Err()
			}
		})
		if err != nil && streamCtx.Err() == nil {
			errs <- err
		}
	}()
	return &eventStream{events: events, errors: errs, close: cancel}, nil
}

func scanSSE(reader io.Reader, emit func(ponsruntime.Event) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var data string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if data != "" {
				var event ponsruntime.Event
				if err := json.Unmarshal([]byte(data), &event); err != nil {
					return fmt.Errorf("runtime HTTP client: decode event: %w", err)
				}
				if err := emit(event); err != nil {
					return err
				}
			}
			data = ""
			continue
		}
		if value, ok := strings.CutPrefix(line, "data:"); ok {
			value = strings.TrimPrefix(value, " ")
			if data == "" {
				data = value
			} else {
				data += "\n" + value
			}
		}
	}
	return scanner.Err()
}

type SendResult struct {
	ConversationID string
	ResponseID     string
	Answer         string
}

// Events opens a replayable live stream. Call View first and pass its
// EventCursor to avoid replaying state already represented in the snapshot.
func (c Client) Events(ctx context.Context, conversationID string, after uint64) (<-chan ponsruntime.Event, <-chan error, func(), error) {
	stream, err := c.openEvents(ctx, conversationID, after)
	if err != nil {
		return nil, nil, nil, err
	}
	return stream.events, stream.errors, stream.close, nil
}

// Send opens the event stream before submitting the message and waits for
// that message's terminal event.
func (c Client) Send(ctx context.Context, conversationID, idempotencyKey, text string, onSnapshot func(ponsruntime.ConversationView), onEvent func(ponsruntime.Event)) (SendResult, error) {
	return c.SendWithOptions(ctx, conversationID, idempotencyKey, text, ponsruntime.ConversationOptions{}, onSnapshot, onEvent)
}

func (c Client) SendWithOptions(ctx context.Context, conversationID, idempotencyKey, text string, options ponsruntime.ConversationOptions, onSnapshot func(ponsruntime.ConversationView), onEvent func(ponsruntime.Event)) (SendResult, error) {
	var snapshot ponsruntime.ConversationView
	after := uint64(0)
	if conversationID == "" {
		conversation, err := c.CreateConversation(ctx, options)
		if err != nil {
			return SendResult{}, err
		}
		conversationID = conversation.ID
	} else {
		var err error
		snapshot, err = c.View(ctx, conversationID)
		if err != nil {
			return SendResult{}, err
		}
		after = snapshot.EventCursor
		if onSnapshot != nil {
			onSnapshot(snapshot)
		}
	}
	stream, err := c.openEvents(ctx, conversationID, after)
	if err != nil {
		return SendResult{}, err
	}
	defer stream.close()
	if idempotencyKey == "" {
		idempotencyKey = ponsruntime.NewID()
	}
	accepted, err := c.Submit(ctx, conversationID, idempotencyKey, []ponsruntime.TextPart{{Type: "text", Text: text}})
	if err != nil {
		return SendResult{}, err
	}
	if accepted.Duplicate {
		if answer, ok := completedAnswer(snapshot.Messages, accepted.InboundMessageID); ok {
			return SendResult{ConversationID: conversationID, ResponseID: answer.id, Answer: answer.text}, nil
		}
		for _, submission := range snapshot.Submissions {
			if submission.ID == accepted.InboundMessageID && submission.Status == ponsruntime.RunFailed {
				return SendResult{}, errors.New(submission.Error)
			}
		}
	}
	errorEvents := stream.errors
	for {
		select {
		case <-ctx.Done():
			return SendResult{}, ctx.Err()
		case err, ok := <-errorEvents:
			if !ok {
				errorEvents = nil
				continue
			}
			if err != nil {
				return SendResult{}, err
			}
		case event, ok := <-stream.events:
			if !ok {
				return SendResult{}, errors.New("runtime HTTP client: event stream closed before completion")
			}
			if event.InboundMessageID != accepted.InboundMessageID {
				continue
			}
			if onEvent != nil {
				onEvent(event)
			}
			switch event.Type {
			case ponsruntime.EventMessageUpserted:
				if event.Message != nil && event.Message.Role == "assistant" && event.Message.Final {
					var answer strings.Builder
					for _, part := range event.Message.Parts {
						if part.Type == "text" {
							answer.WriteString(part.Text)
						}
					}
					return SendResult{ConversationID: conversationID, ResponseID: event.Message.ID, Answer: answer.String()}, nil
				}
			case ponsruntime.EventRunUpdated:
				if event.Run != nil && event.Run.Status == ponsruntime.RunFailed {
					return SendResult{}, errors.New(event.Run.Error)
				}
			}
		}
	}
}

type answerMessage struct{ id, text string }

func completedAnswer(messages []ponsruntime.Message, inboundMessageID string) (answerMessage, bool) {
	for _, message := range messages {
		if message.InboundMessageID != inboundMessageID || message.Role != "assistant" || !message.Final {
			continue
		}
		var answer strings.Builder
		for _, part := range message.Parts {
			if part.Type == "text" {
				answer.WriteString(part.Text)
			}
		}
		return answerMessage{id: message.ID, text: answer.String()}, true
	}
	return answerMessage{}, false
}

func responseError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body map[string]string
	if json.Unmarshal(data, &body) == nil && body["error"] != "" {
		return fmt.Errorf("runtime HTTP client: HTTP %d: %s", resp.StatusCode, body["error"])
	}
	return fmt.Errorf("runtime HTTP client: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}
