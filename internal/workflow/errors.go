package workflow

import "errors"

var (
	// ErrNondeterministic reports that replay diverged from the recorded
	// history: the workflow function made a different call, or made calls in a
	// different order, than it did originally. This is failure F6.3 and it
	// means the determinism contract of ADR-001 was broken.
	ErrNondeterministic = errors.New("workflow: nondeterministic replay")

	// ErrInputMismatch reports that a replayed activity was called with
	// different input than was recorded. The idempotency key is unchanged
	// (ADR-003 derives it from position, not content), so no duplicate effect
	// is created — but the divergence is real and is surfaced rather than
	// silently accepted.
	ErrInputMismatch = errors.New("workflow: replayed input differs from recorded input")

	// ErrIllegalTransition reports an attempted state transition that the
	// specification forbids. This is failure F6.4: a bug in the runtime, not an
	// environmental fault. A benchmark run that produces one is invalid.
	ErrIllegalTransition = errors.New("workflow: illegal state transition")

	// ErrEffectUnresolved reports that the workflow tried to complete while an
	// effect was still UNKNOWN. Invariant I12.
	ErrEffectUnresolved = errors.New("workflow: cannot complete with an unresolved effect")

	// ErrResponseDropped simulates failure F2.3: the request was received and
	// committed by the remote system, and the response was lost. The caller
	// cannot distinguish this from F2.1 (never arrived) or F2.2 (still
	// processing) — which is the entire problem this project studies.
	ErrResponseDropped = errors.New("workflow: response dropped after remote commit")
)

// definiteFailure is implemented by errors that report a conclusive outcome:
// the external system considered the request and created nothing.
//
// The interface is structural, so a tool adapter can report definiteness
// without importing this package.
//
// Most failures are not definite. A timeout, a reset connection or a dead
// worker all leave the outcome genuinely unknown, and those must be recorded as
// UNKNOWN. Only an explicit rejection — a 400, a validation error, an answer
// from the service saying it did nothing — justifies FAILED.
//
// The default is deliberately the conservative one: an unrecognised error is
// treated as UNKNOWN. That costs a reconciliation we might not have needed,
// which is a far better trade than a duplicate charge.
type definiteFailure interface {
	DefiniteFailure() bool
}

// IsDefiniteFailure reports whether err conclusively means no effect occurred.
func IsDefiniteFailure(err error) bool {
	var d definiteFailure
	if errors.As(err, &d) {
		return d.DefiniteFailure()
	}
	return false
}
