package actionpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/brain/llm"
)

// CodexClassifier assesses actions through a subscription-backed Codex client.
// Its confidence is model-generated, like the OpenAI API classifier's.
type CodexClassifier struct {
	client  llm.Client
	model   string
	timeout time.Duration
}

func NewCodexClassifier(client llm.Client, model string, timeout time.Duration) (*CodexClassifier, error) {
	if client == nil || model == "" {
		return nil, errors.New("actionpolicy: Codex classifier requires a client and model")
	}
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if timeout < 0 {
		return nil, errors.New("actionpolicy: classifier timeout must be positive")
	}
	return &CodexClassifier{client: client, model: model, timeout: timeout}, nil
}

func (c *CodexClassifier) Assess(ctx context.Context, req pons.ToolCallStartInput) (pons.ActionAssessment, error) {
	state, err := json.Marshal(classifierState(req))
	if err != nil {
		return pons.ActionAssessment{}, fmt.Errorf("actionpolicy: encode classifier state: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	turn, err := c.client.Complete(ctx, openAIInstructions+" Return only a JSON object with risk, confidence, and reason_code.",
		[]llm.Turn{{Role: "user", Blocks: []llm.Block{llm.Text{Value: string(state)}}}}, nil)
	if err != nil {
		return pons.ActionAssessment{}, fmt.Errorf("actionpolicy: Codex classifier request failed: %w", err)
	}
	var output string
	for _, block := range turn.Blocks {
		switch block := block.(type) {
		case llm.Raw:
			continue // reasoning state is not assessment text
		case llm.Text:
			if output != "" {
				return pons.ActionAssessment{}, errors.New("actionpolicy: invalid Codex assessment")
			}
			output = block.Value
		default:
			return pons.ActionAssessment{}, errors.New("actionpolicy: invalid Codex assessment")
		}
	}
	if output == "" || len(output) > maxClassifierResponse {
		return pons.ActionAssessment{}, errors.New("actionpolicy: invalid Codex assessment")
	}
	var result struct {
		Risk       string  `json:"risk"`
		Confidence float64 `json:"confidence"`
		ReasonCode string  `json:"reason_code"`
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return pons.ActionAssessment{}, errors.New("actionpolicy: invalid Codex assessment")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return pons.ActionAssessment{}, errors.New("actionpolicy: invalid Codex assessment")
	}
	assessment := pons.ActionAssessment{
		Risk: result.Risk, Confidence: result.Confidence,
		ReasonCode: result.ReasonCode, Classifier: "codex/" + c.model,
	}
	if !validAssessment(assessment) {
		return pons.ActionAssessment{}, errors.New("actionpolicy: invalid Codex assessment")
	}
	return assessment, nil
}
