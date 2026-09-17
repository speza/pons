package llm

import (
	"context"
	"errors"
	"testing"

	"github.com/samperrin/pons"
)

type fallbackFakeClient struct {
	err   error
	turn  Turn
	calls int
}

func (f *fallbackFakeClient) Complete(ctx context.Context, system string, turns []Turn, tools []pons.ToolSpec) (Turn, error) {
	f.calls++
	return f.turn, f.err
}

func TestFailoverTriesSlotsInOrder(t *testing.T) {
	failing := &fallbackFakeClient{err: errors.New("rate limited")}
	backup := &fallbackFakeClient{turn: Turn{Role: "assistant", Blocks: []Block{Text{Value: "ok"}}}}
	f := &failoverClient{
		clients: []Client{failing, backup},
		names:   []string{"a/m1", "b/m2"},
	}
	turn, err := f.Complete(context.Background(), "sys",
		[]Turn{{Role: "user", Blocks: []Block{Text{Value: "go"}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if failing.calls != 1 || backup.calls != 1 {
		t.Fatalf("calls: primary=%d backup=%d", failing.calls, backup.calls)
	}
	if len(turn.Blocks) != 1 || turn.Blocks[0].(Text).Value != "ok" {
		t.Fatalf("turn: %+v", turn.Blocks)
	}
}

func TestFailoverAllFailReturnsLastError(t *testing.T) {
	first := &fallbackFakeClient{err: errors.New("429")}
	second := &fallbackFakeClient{err: errors.New("outage")}
	f := &failoverClient{clients: []Client{first, second}, names: []string{"a", "b"}}
	_, err := f.Complete(context.Background(), "sys", nil, nil)
	if err == nil || err.Error() != "outage" {
		t.Fatalf("want last error, got %v", err)
	}
	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("calls: %d %d", first.calls, second.calls)
	}
}

func TestFailoverStopsOnCancellation(t *testing.T) {
	failing := &fallbackFakeClient{err: context.Canceled}
	backup := &fallbackFakeClient{turn: Turn{Role: "assistant"}}
	f := &failoverClient{clients: []Client{failing, backup}, names: []string{"a", "b"}}
	_, err := f.Complete(t.Context(), "sys", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want cancellation, got %v", err)
	}
	if backup.calls != 0 {
		t.Fatalf("cancellation should abort the chain: backup called %d times", backup.calls)
	}
}

func TestNewBuildsFailoverChain(t *testing.T) {
	b, err := New(Config{
		Provider: "anthropic",
		APIKey:   "test",
		Fallbacks: []Fallback{
			{Provider: "openai", APIKey: "k2", Model: "gpt-x"},
			{Provider: "openai", APIKey: "k3"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fc, ok := b.client.(*failoverClient)
	if !ok {
		t.Fatalf("want failoverClient, got %T", b.client)
	}
	if len(fc.clients) != 3 {
		t.Fatalf("slots: %d", len(fc.clients))
	}
	if _, ok := fc.clients[1].(*openaiClient); !ok {
		t.Fatalf("fallback 1: %T", fc.clients[1])
	}
	if got := fc.clients[1].(*openaiClient).model; got != "gpt-x" {
		t.Fatalf("fallback model: %q", got)
	}
	if b.model != "claude-sonnet-4-5" {
		t.Fatalf("primary model default: %q", b.model)
	}
}

func TestNewRejectsBrokenFallback(t *testing.T) {
	if _, err := New(Config{
		Provider:  "anthropic",
		APIKey:    "test",
		Fallbacks: []Fallback{{Provider: "nope"}},
	}); err == nil {
		t.Fatal("unknown fallback provider should fail composition")
	}
	if _, err := New(Config{
		Provider:  "anthropic",
		APIKey:    "test",
		Fallbacks: []Fallback{{Provider: "openai", APIKey: "k"}},
	}); err != nil {
		t.Fatalf("fallback without env keys is fine when APIKey given: %v", err)
	}
}
