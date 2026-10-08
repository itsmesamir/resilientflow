// Package fake provides model implementations that make the runtime's
// semantics testable without a network or a bill.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/itsmesamir/resilientflow/adapters/model"
)

// Deterministic is a model whose output is a pure function of its input.
//
// This is deliberately *not* realistic — a real model is nondeterministic, and
// Phase 9 introduces one that behaves accordingly. Determinism here is a
// testing instrument: it lets a replay be compared against an original run and
// any divergence attributed to the runtime rather than to the model.
type Deterministic struct {
	mu    sync.Mutex
	calls int
}

func NewDeterministic() *Deterministic { return &Deterministic{} }

func (m *Deterministic) Generate(_ context.Context, req model.Request) (model.Response, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()

	sum := sha256.Sum256([]byte(req.Prompt))
	digest := hex.EncodeToString(sum[:8])

	return model.Response{
		Text:         fmt.Sprintf("decision:%s", digest),
		InputTokens:  len(req.Prompt) / 4,
		OutputTokens: 16,
		RequestID:    "det-" + digest,
	}, nil
}

// Calls reports how many times the model was actually invoked.
//
// A replay that reuses recorded output must leave this unchanged. That
// assertion is the whole point of the memoization path in Execute.
func (m *Deterministic) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *Deterministic) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = 0
}

// Faulty wraps a model and fails according to a fixed, seeded schedule.
//
// The schedule is explicit rather than probabilistic so that an experiment is
// reproducible: "fail the second and third calls" is a fact about a run, where
// "fail 30% of calls" is only a fact about a distribution.
type Faulty struct {
	Inner model.Model

	// FailOn lists 1-based call numbers that return Err instead of a response.
	FailOn map[int]bool

	// Err is returned for failing calls. Defaults to a rate-limit error, which
	// is failure F4.3.
	Err error

	mu    sync.Mutex
	calls int
}

// ErrRateLimited is failure F4.3: the provider refused the call.
var ErrRateLimited = fmt.Errorf("model: rate limited (429)")

func NewFaulty(inner model.Model, failOn ...int) *Faulty {
	set := make(map[int]bool, len(failOn))
	for _, n := range failOn {
		set[n] = true
	}
	return &Faulty{Inner: inner, FailOn: set, Err: ErrRateLimited}
}

func (m *Faulty) Generate(ctx context.Context, req model.Request) (model.Response, error) {
	m.mu.Lock()
	m.calls++
	n := m.calls
	m.mu.Unlock()

	if m.FailOn[n] {
		return model.Response{}, m.Err
	}
	return m.Inner.Generate(ctx, req)
}

func (m *Faulty) Calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *Faulty) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = 0
}
