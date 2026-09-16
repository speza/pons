// OpenAI adapter: thin translation between the harness Block model and the
// official openai-go SDK (Chat Completions — the widely-compatible wire
// format: OpenAI, Groq, OpenRouter, llama.cpp, Ollama, vLLM, …).
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go"

	oopt "github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/shared"
	"github.com/samperrin/pons"
)

// openaiClient implements Client via the official SDK.
type openaiClient struct {
	client    *openai.Client
	model     string
	maxTokens int
}

func newOpenAIClient(key, baseURL string, maxTokens int) *openaiClient {
	opts := []oopt.RequestOption{
		oopt.WithMaxRetries(0),
		oopt.WithHTTPClient(defaultHTTPClient()),
	}
	if key != "" {
		opts = append(opts, oopt.WithAPIKey(key))
	}
	if baseURL != "" {
		opts = append(opts, oopt.WithBaseURL(baseURL))
	}
	return &openaiClient{client: func() *openai.Client { c := openai.NewClient(opts...); return &c }(),
		model: "gpt-4o-mini", maxTokens: maxTokens}
}

func (c *openaiClient) Complete(ctx context.Context, system string, turns []Turn, tools []pons.ToolSpec) (Turn, error) {
	params := openai.ChatCompletionNewParams{
		Model:               shared.ChatModel(c.model),
		MaxCompletionTokens: param.NewOpt(int64(c.maxTokens)),
		Messages: []openai.ChatCompletionMessageParamUnion{
			{OfSystem: &openai.ChatCompletionSystemMessageParam{
				Content: openai.ChatCompletionSystemMessageParamContentUnion{OfString: param.NewOpt(system)},
			}},
		},
	}
	for _, spec := range tools {
		params.Tools = append(params.Tools, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        string(spec.Kind),
				Description: param.NewOpt(spec.Description),
				Parameters:  jsonSchema(spec),
			},
		})
	}
	for _, t := range turns {
		msgs, err := chatMessages(t)
		if err != nil {
			return Turn{}, err
		}
		params.Messages = append(params.Messages, msgs...)
	}

	completion, err := c.client.Chat.Completions.New(ctx, params)
	if err != nil {
		return Turn{}, fmt.Errorf("openai: %w", err)
	}
	if len(completion.Choices) == 0 {
		return Turn{}, fmt.Errorf("openai: empty choices")
	}
	if completion.Choices[0].FinishReason == "length" {
		return Turn{}, fmt.Errorf("openai: response truncated at max_tokens (%d) — raise Config.MaxTokens or the model needs a smaller step", c.maxTokens)
	}

	out := Turn{Role: "assistant"}
	msg := completion.Choices[0].Message
	if msg.Content != "" {
		out.Blocks = append(out.Blocks, Text{Value: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		input := decodeToolInput([]byte(tc.Function.Arguments))
		out.Blocks = append(out.Blocks, ToolUse{ID: tc.ID, Name: tc.Function.Name, Input: input})
	}
	return out, nil
}

func chatMessages(t Turn) ([]openai.ChatCompletionMessageParamUnion, error) {
	switch t.Role {
	case "user":
		var msgs []openai.ChatCompletionMessageParamUnion
		var text []string
		for _, b := range t.Blocks {
			switch blk := b.(type) {
			case Text:
				text = append(text, blk.Value)
			case Result:
				msgs = append(msgs, openai.ChatCompletionMessageParamUnion{OfTool: &openai.ChatCompletionToolMessageParam{
					ToolCallID: blk.ToolUseID,
					Content:    openai.ChatCompletionToolMessageParamContentUnion{OfString: param.NewOpt(blk.Content)},
				}})
			}
		}
		if len(text) > 0 {
			msgs = append(msgs, openai.ChatCompletionMessageParamUnion{OfUser: &openai.ChatCompletionUserMessageParam{
				Content: openai.ChatCompletionUserMessageParamContentUnion{OfString: param.NewOpt(strings.Join(text, "\n"))},
			}})
		}
		return msgs, nil
	case "assistant":
		m := &openai.ChatCompletionAssistantMessageParam{}
		var text []string
		for _, b := range t.Blocks {
			switch blk := b.(type) {
			case Text:
				text = append(text, blk.Value)
			case ToolUse:
				args, err := json.Marshal(blk.Input)
				if err != nil {
					return nil, err
				}
				m.ToolCalls = append(m.ToolCalls, openai.ChatCompletionMessageToolCallParam{
					ID:       blk.ID,
					Type:     "function",
					Function: openai.ChatCompletionMessageToolCallFunctionParam{Name: blk.Name, Arguments: string(args)},
				})
			}
		}
		if len(text) > 0 {
			m.Content = openai.ChatCompletionAssistantMessageParamContentUnion{OfString: param.NewOpt(strings.Join(text, "\n"))}
		}
		return []openai.ChatCompletionMessageParamUnion{{OfAssistant: m}}, nil
	}
	return nil, fmt.Errorf("openai: unknown turn role %q", t.Role)
}
