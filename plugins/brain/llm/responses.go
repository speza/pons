// Responses API adapter — the Codex wire format — via the official
// openai-go SDK (streaming SSE included). Works against:
//
//   - the ChatGPT-subscription backend (provider "codex"): base URL
//     https://chatgpt.com/backend-api/codex, Bearer token + ChatGPT-Account-ID
//     codex auth-file auth, refreshed in memory when expired/401
//   - api.openai.com/v1/responses with a plain API key (provider
//     "openai-responses")
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"
	"github.com/openai/openai-go/shared/constant"

	"github.com/samperrin/pons"
)

// responsesClient implements Client via the openai-go Responses API.
type responsesClient struct {
	model     string
	maxTokens int
	auth      *codexAuth // subscription mode (nil in API-key mode)
	apiKey    string     // API-key mode
	baseURL   string
}

func (c *responsesClient) Complete(ctx context.Context, system string, turns []Turn, tools []pons.ToolSpec) (Turn, error) {
	// Subscription tokens expire hourly: refresh before the call if expired,
	// and once more on a 401 before giving up.
	if c.auth != nil {
		if _, _, err := c.auth.current(ctx); err != nil {
			return Turn{}, err
		}
	}
	turn, err := c.tryComplete(ctx, system, turns, tools)
	if err != nil && c.auth != nil && isUnauthorized(err) {
		if rerr := c.auth.refresh(ctx); rerr != nil {
			return Turn{}, fmt.Errorf("codex: HTTP 401 and token refresh failed (%v) — run pons -provider codex --login", rerr)
		}
		return c.tryComplete(ctx, system, turns, tools)
	}
	return turn, err
}

func isUnauthorized(err error) bool {
	var apiErr *openai.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == 401
}

func (c *responsesClient) tryComplete(ctx context.Context, system string, turns []Turn, tools []pons.ToolSpec) (Turn, error) {
	opts := []option.RequestOption{
		option.WithMaxRetries(0),
		option.WithHTTPClient(defaultHTTPClient()),
		option.WithBaseURL(c.baseURL),
	}
	token := c.apiKey
	accountID := ""
	if c.auth != nil {
		token = c.auth.Access // current() validated/refreshed before the call
		accountID = c.auth.AccountID
	}
	opts = append(opts, option.WithAPIKey(token))
	if accountID != "" {
		opts = append(opts, option.WithHeader("chatgpt-account-id", accountID))
	}
	params := responses.ResponseNewParams{
		Model:             shared.ResponsesModel(c.model),
		Instructions:      param.NewOpt(system),
		ParallelToolCalls: param.NewOpt(false),
		Store:             param.NewOpt(false), // stateless: full context replayed each turn
		Include:           []responses.ResponseIncludable{"reasoning.encrypted_content"},
		Input:             responses.ResponseNewParamsInputUnion{OfInputItemList: []responses.ResponseInputItemUnionParam{}},
	}
	// The ChatGPT-subscription backend rejects max_output_tokens ("unsupported
	// parameter") — it manages output limits itself. API-key mode may set it.
	if c.auth == nil && c.maxTokens > 0 {
		params.MaxOutputTokens = param.NewOpt(int64(c.maxTokens))
	}
	for _, spec := range tools {
		params.Tools = append(params.Tools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        string(spec.Kind),
				Description: param.NewOpt(spec.Description),
				Strict:      param.NewOpt(false),
				Parameters:  jsonSchema(spec),
				Type:        "function",
			},
		})
	}
	for _, t := range turns {
		items, err := responsesInputItems(t)
		if err != nil {
			return Turn{}, err
		}
		params.Input.OfInputItemList = append(params.Input.OfInputItemList, items...)
	}

	// Capture the API's raw error payload: without it, provider 400s are
	// undiagnosable ("Bad Request" with no detail).
	var errBody string
	capture := func(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		resp, err := next(req)
		if err == nil && resp.StatusCode >= 400 {
			b, rerr := io.ReadAll(resp.Body)
			if rerr == nil {
				errBody = strings.TrimSpace(string(b))
				resp.Body = io.NopCloser(strings.NewReader(string(b)))
			}
		}
		return resp, err
	}
	opts = append(opts, option.WithMiddleware(capture))
	client := openai.NewClient(opts...)

	stream := client.Responses.NewStreaming(ctx, params)
	var items []responses.ResponseOutputItemUnion
	var completed *responses.ResponseCompletedEvent
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		// The ChatGPT-subscription backend sends an empty output[] on
		// response.completed — the real items arrive via output_item.done
		// (Codex CLI accumulates them the same way).
		case "response.output_item.done":
			done := ev.AsResponseOutputItemDone()
			items = append(items, done.Item)
		case "response.completed":
			done := ev.AsResponseCompleted()
			completed = &done
			if len(done.Response.Output) > 0 {
				items = done.Response.Output
			}
		case "response.failed":
			f := ev.AsResponseFailed()
			msg := "stream failed"
			if f.Response.Error.Message != "" {
				msg = f.Response.Error.Message
			}
			return Turn{}, fmt.Errorf("codex: %s", msg)
		case "error":
			return Turn{}, fmt.Errorf("codex: %s", ev.AsError().Message)
		}
	}
	if err := stream.Err(); err != nil {
		if errBody != "" {
			return Turn{}, fmt.Errorf("codex: %w: %s", err, truncateMsg(errBody, 512))
		}
		return Turn{}, fmt.Errorf("codex: %w", err)
	}
	// Truncated response = the model planned more work than arrived.
	if completed != nil && completed.Response.Status == "incomplete" {
		return Turn{}, fmt.Errorf("codex: response incomplete (%s) — raise Config.MaxTokens or the model needs a smaller step", completed.Response.IncompleteDetails.Reason)
	}
	if items == nil {
		return Turn{}, fmt.Errorf("codex: stream ended without any output items")
	}
	return responseToTurn(items), nil
}

func responseToTurn(output []responses.ResponseOutputItemUnion) Turn {
	out := Turn{Role: "assistant"}
	for _, item := range output {
		switch item.Type {
		case "function_call":
			fc := item.AsFunctionCall()
			input := decodeToolInput([]byte(fc.Arguments))
			out.Blocks = append(out.Blocks, ToolUse{ID: fc.CallID, Name: fc.Name, Input: input})
		case "message":
			m := item.AsMessage()
			var sb strings.Builder
			for _, part := range m.Content {
				sb.WriteString(part.Text)
			}
			out.Blocks = append(out.Blocks, Text{Value: sb.String()})
		case "reasoning":
			r := item.AsReasoning()
			// Replay reasoning between turns keeps model state coherent with
			// store:false — but only when the encrypted payload came back.
			if r.ID == "" || r.EncryptedContent == "" {
				continue
			}
			raw, _ := json.Marshal(map[string]any{
				"type": "reasoning", "id": r.ID,
				"summary": r.Summary, "encrypted_content": r.EncryptedContent,
			})
			out.Blocks = append(out.Blocks, Raw{Item: raw})
		default:
			continue // web_search_call etc.: not ours to replay
		}
	}
	return out
}

// responsesInputItems translates one internal Turn into SDK input items.
func responsesInputItems(t Turn) ([]responses.ResponseInputItemUnionParam, error) {
	var out []responses.ResponseInputItemUnionParam
	for _, b := range t.Blocks {
		switch blk := b.(type) {
		case Text:
			// Explicit message items (EasyInputMessage elides "type", which
			// the Codex backend rejects).
			role, contentType := "user", constant.InputText("input_text")
			if t.Role == "assistant" {
				role = "assistant"
				contentType = constant.InputText("output_text")
			}
			out = append(out, responses.ResponseInputItemUnionParam{
				OfInputMessage: &responses.ResponseInputItemMessageParam{
					Type: "message",
					Role: role,
					Content: []responses.ResponseInputContentUnionParam{{
						OfInputText: &responses.ResponseInputTextParam{Type: contentType, Text: blk.Value},
					}},
				},
			})
		case ToolUse:
			args, err := json.Marshal(blk.Input)
			if err != nil {
				return nil, err
			}
			out = append(out, responses.ResponseInputItemUnionParam{
				OfFunctionCall: &responses.ResponseFunctionToolCallParam{
					CallID: blk.ID, Name: blk.Name, Arguments: string(args),
				},
			})
		case Result:
			out = append(out, responses.ResponseInputItemUnionParam{
				OfFunctionCallOutput: &responses.ResponseInputItemFunctionCallOutputParam{
					CallID: blk.ToolUseID, Output: blk.Content,
				},
			})
		case Raw:
			var item responses.ResponseInputItemUnionParam
			if err := json.Unmarshal(blk.Item, &item); err != nil {
				return nil, fmt.Errorf("codex: replay reasoning item: %w", err)
			}
			out = append(out, item)
		}
	}
	return out, nil
}
