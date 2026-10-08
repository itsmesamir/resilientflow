// Package model defines the boundary between the runtime and a language model.
//
// The runtime treats a model as what it is: an unreliable, expensive,
// nondeterministic remote service. Everything already known about remote calls
// therefore applies — timeouts, retries, idempotency, cost accounting — and
// nothing about it is special.
//
// Benchmarks must never depend on a paid provider. The deterministic fake is
// the default; a real adapter is optional and is never required to reproduce a
// result.
package model

import "context"

// Request is one model call.
type Request struct {
	Prompt      string  `json:"prompt"`
	MaxTokens   int     `json:"max_tokens,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
}

// Response carries the output and the cost metadata the benchmark needs.
//
// Token counts are recorded because "LLM calls replayed" and "token cost
// wasted" are headline metrics: the economic argument for selective replay
// rests on them.
type Response struct {
	Text         string `json:"text"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`

	// RequestID is the provider's identifier for the call, where one exists. It
	// is the only thing that makes an uncertain model call reconcilable.
	RequestID string `json:"request_id,omitempty"`
}

// Model is the single seam through which the runtime reaches a model.
type Model interface {
	Generate(ctx context.Context, req Request) (Response, error)
}

// Calls counts invocations. Implementations embed it so tests can assert that
// a replay made zero fresh model calls — the central claim of Phase 9.
type Counter interface {
	Calls() int
	Reset()
}
