package actionpolicy

import (
	"context"
	"strings"
	"testing"

	"github.com/samperrin/pons"
)

func TestClassifierPluginResolvesIntoPolicy(t *testing.T) {
	classifier := ClassifierFunc(func(context.Context, pons.ToolCallStartEvent) (pons.ActionAssessment, error) {
		return pons.ActionAssessment{Risk: "safe", Confidence: 0.99, ProbabilityConfidence: true}, nil
	})
	core := pons.New()
	if err := core.Use(ClassifierPlugin{ID: "example", Classifier: classifier}, Policy{ClassifierID: "example"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := core.Capability(ClassifierCapability("example")); !ok {
		t.Fatal("classifier capability was not registered")
	}
	if err := core.Use(ClassifierPlugin{ID: "example", Classifier: classifier}); err == nil {
		t.Fatal("duplicate classifier capability was accepted")
	}
}

func TestPolicyRequiresRegisteredClassifier(t *testing.T) {
	core := pons.New()
	if err := core.Use(Policy{ClassifierID: "missing"}); err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("missing classifier error = %v", err)
	}
	if err := core.RegisterCapability(ClassifierCapability("wrong"), "wrong type"); err != nil {
		t.Fatal(err)
	}
	if err := core.Use(Policy{ClassifierID: "wrong"}); err == nil || !strings.Contains(err.Error(), "not a classifier") {
		t.Fatalf("wrong classifier type error = %v", err)
	}
}
