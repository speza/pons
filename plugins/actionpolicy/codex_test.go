package actionpolicy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/brain/llm"
)

type classifierClientFunc func(context.Context, string, []llm.Turn, []pons.ToolSpec) (llm.Turn, error)

func (f classifierClientFunc) Complete(ctx context.Context, system string, turns []llm.Turn, tools []pons.ToolSpec) (llm.Turn, error) {
	return f(ctx, system, turns, tools)
}

func TestCodexClassifierAssessment(t *testing.T) {
	client := classifierClientFunc(func(_ context.Context, system string, turns []llm.Turn, tools []pons.ToolSpec) (llm.Turn, error) {
		if !strings.Contains(system, "source-labeled context") || len(tools) != 0 || len(turns) != 1 {
			t.Fatal("classifier request shape")
		}
		text := turns[0].Blocks[0].(llm.Text).Value
		if !strings.Contains(text, "git push origin dev") || !strings.Contains(text, "push it") {
			t.Fatalf("classifier state missing action or context: %s", text)
		}
		return llm.Turn{Role: "assistant", Blocks: []llm.Block{
			llm.Raw{}, llm.Text{Value: `{"risk":"safe","confidence":0.98,"reason_code":"user_approved"}`},
		}}, nil
	})
	classifier, err := NewCodexClassifier(client, "gpt-6-luna", 0)
	if err != nil {
		t.Fatal(err)
	}
	assessment, err := classifier.Assess(context.Background(), testRequest())
	if err != nil || assessment.Classifier != "codex/gpt-6-luna" || assessment.Risk != "safe" ||
		assessment.Confidence != 0.98 || assessment.ReasonCode != "user_approved" || assessment.ProbabilityConfidence {
		t.Fatalf("assessment=%+v err=%v", assessment, err)
	}
}

func TestCodexClassifierInvalidResponse(t *testing.T) {
	for _, response := range []string{
		`{"risk":"safe","confidence":0.99,"reason_code":"ok"} trailing`,
		`{"risk":"safe","confidence":2,"reason_code":"ok"}`,
		"```json\n{}\n```",
	} {
		client := classifierClientFunc(func(context.Context, string, []llm.Turn, []pons.ToolSpec) (llm.Turn, error) {
			return llm.Turn{Blocks: []llm.Block{llm.Text{Value: response}}}, nil
		})
		classifier, _ := NewCodexClassifier(client, "gpt-6-luna", 0)
		if _, err := classifier.Assess(context.Background(), testRequest()); err == nil || !strings.Contains(err.Error(), "invalid Codex assessment") {
			t.Fatalf("response %q: err=%v", response, err)
		}
	}
	client := classifierClientFunc(func(context.Context, string, []llm.Turn, []pons.ToolSpec) (llm.Turn, error) {
		return llm.Turn{}, errors.New("provider failed")
	})
	classifier, _ := NewCodexClassifier(client, "gpt-6-luna", 0)
	if _, err := classifier.Assess(context.Background(), testRequest()); err == nil {
		t.Fatal("provider error was ignored")
	}
}
