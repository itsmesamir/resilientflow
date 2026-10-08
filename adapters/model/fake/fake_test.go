package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/itsmesamir/resilientflow/adapters/model"
)

func TestDeterministicIsAFunctionOfItsInput(t *testing.T) {
	m := NewDeterministic()
	ctx := context.Background()

	a, err := m.Generate(ctx, model.Request{Prompt: "hello"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	b, err := m.Generate(ctx, model.Request{Prompt: "hello"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if a.Text != b.Text {
		t.Fatalf("same prompt gave %q then %q", a.Text, b.Text)
	}

	c, err := m.Generate(ctx, model.Request{Prompt: "different"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if c.Text == a.Text {
		t.Fatal("different prompts must give different output")
	}
}

func TestDeterministicCountsCalls(t *testing.T) {
	m := NewDeterministic()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := m.Generate(ctx, model.Request{Prompt: "p"}); err != nil {
			t.Fatalf("generate: %v", err)
		}
	}
	if m.Calls() != 3 {
		t.Fatalf("counted %d calls, want 3", m.Calls())
	}

	m.Reset()
	if m.Calls() != 0 {
		t.Fatal("Reset did not clear the counter")
	}
}

func TestDeterministicReportsTokens(t *testing.T) {
	m := NewDeterministic()
	resp, err := m.Generate(context.Background(), model.Request{Prompt: "a longer prompt here"})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if resp.OutputTokens <= 0 {
		t.Error("output tokens must be reported; the cost argument depends on them")
	}
	if resp.RequestID == "" {
		t.Error("a request ID is the only thing that makes an uncertain call reconcilable")
	}
}

// TestFaultyFollowsItsSchedule: faults are scheduled, not probabilistic, so an
// experiment is reproducible. "Fail the second call" is a fact about a run;
// "fail 30% of calls" is only a fact about a distribution.
func TestFaultyFollowsItsSchedule(t *testing.T) {
	m := NewFaulty(NewDeterministic(), 2, 3)
	ctx := context.Background()

	if _, err := m.Generate(ctx, model.Request{Prompt: "p"}); err != nil {
		t.Fatalf("call 1 should succeed: %v", err)
	}
	if _, err := m.Generate(ctx, model.Request{Prompt: "p"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("call 2 should be rate limited, got %v", err)
	}
	if _, err := m.Generate(ctx, model.Request{Prompt: "p"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("call 3 should be rate limited, got %v", err)
	}
	if _, err := m.Generate(ctx, model.Request{Prompt: "p"}); err != nil {
		t.Fatalf("call 4 should succeed: %v", err)
	}

	if m.Calls() != 4 {
		t.Fatalf("counted %d calls, want 4", m.Calls())
	}
}

func TestFaultyIsReproducible(t *testing.T) {
	run := func() []bool {
		m := NewFaulty(NewDeterministic(), 2)
		var outcomes []bool
		for i := 0; i < 4; i++ {
			_, err := m.Generate(context.Background(), model.Request{Prompt: "p"})
			outcomes = append(outcomes, err == nil)
		}
		return outcomes
	}

	first, second := run(), run()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("call %d differed between identical runs", i+1)
		}
	}
}

// Compile-time checks that both fakes satisfy the interfaces the runtime uses.
var (
	_ model.Model   = (*Deterministic)(nil)
	_ model.Counter = (*Deterministic)(nil)
	_ model.Model   = (*Faulty)(nil)
	_ model.Counter = (*Faulty)(nil)
)
