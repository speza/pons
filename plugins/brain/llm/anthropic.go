// Anthropic adapter: thin translation between the harness Block model and
// the official Anthropic Go SDK (Messages API). Wire-format handling,
// SSE, retries — all owned by the SDK; this file only converts.
package llm

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	aopt "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/samperrin/pons"
)

// anthropicClient implements Client via the official SDK.
type anthropicClient struct {
	client    *anthropic.Client
	model     string
	maxTokens int
}

func newAnthropicClient(key, baseURL string, maxTokens int, extra ...aopt.RequestOption) *anthropicClient {
	opts := []aopt.RequestOption{
		aopt.WithAPIKey(key),
		aopt.WithMaxRetries(0),
		aopt.WithHTTPClient(defaultHTTPClient()),
	}
	opts = append(opts, extra...)
	if baseURL != "" {
		opts = append(opts, aopt.WithBaseURL(baseURL))
	}
	c := anthropic.NewClient(opts...)
	return &anthropicClient{client: &c, model: "claude-sonnet-4-5", maxTokens: maxTokens}
}

func (c *anthropicClient) Complete(ctx context.Context, system string, turns []Turn, tools []pons.ToolSpec) (Turn, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: int64(c.maxTokens),
		System:    []anthropic.TextBlockParam{{Text: system}},
	}
	for _, spec := range tools {
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{
			OfTool: &anthropic.ToolParam{
				Name:        string(spec.Kind),
				Description: param.NewOpt(spec.Description),
				InputSchema: jsonSchemaAnthropic(spec),
			},
		})
	}
	for _, t := range turns {
		m := anthropic.MessageParam{Role: anthropic.MessageParamRole(t.Role)}
		for _, b := range t.Blocks {
			switch blk := b.(type) {
			case Text:
				m.Content = append(m.Content, anthropic.NewTextBlock(blk.Value))
			case ToolUse:
				m.Content = append(m.Content, anthropic.NewToolUseBlock(blk.ID, blk.Input, blk.Name))
			case Result:
				m.Content = append(m.Content, anthropic.NewToolResultBlock(blk.ToolUseID, blk.Content, blk.IsError))
			case Raw:
				// never emitted by this adapter; nothing to replay
			}
		}
		params.Messages = append(params.Messages, m)
	}

	msg, err := c.client.Messages.New(ctx, params)
	if err != nil {
		return Turn{}, fmt.Errorf("anthropic: %w", err)
	}
	// A truncated response is a loop-correctness event, not a normal result:
	// the model may have planned more work than arrived. Surface it.
	switch msg.StopReason {
	case "max_tokens":
		return Turn{}, fmt.Errorf("anthropic: response truncated at max_tokens (%d) — raise Config.MaxTokens or the model needs a smaller step", c.maxTokens)
	case "refusal":
		return Turn{}, fmt.Errorf("anthropic: model refused the request")
	}

	out := Turn{Role: "assistant"}
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			out.Blocks = append(out.Blocks, Text{Value: block.Text})
		case "tool_use":
			input := decodeToolInput(block.Input)
			out.Blocks = append(out.Blocks, ToolUse{ID: block.ID, Name: block.Name, Input: input})
		}
	}
	return out, nil
}

// jsonSchemaAnthropic renders a registered tool's input_schema.
func jsonSchemaAnthropic(spec pons.ToolSpec) anthropic.ToolInputSchemaParam {
	schema := jsonSchema(spec)
	props := schema["properties"]
	var required []string
	if values, ok := schema["required"].([]any); ok {
		for _, value := range values {
			if name, ok := value.(string); ok {
				required = append(required, name)
			}
		}
	} else if values, ok := schema["required"].([]string); ok {
		required = append(required, values...)
	}
	extra := make(map[string]any)
	for key, value := range schema {
		if key != "type" && key != "properties" && key != "required" {
			extra[key] = value
		}
	}
	return anthropic.ToolInputSchemaParam{
		Type:        "object",
		Properties:  props,
		Required:    required,
		ExtraFields: extra,
	}
}
