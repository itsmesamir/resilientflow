package workflow

import "fmt"

// Class is an activity's replay semantics: what the runtime is permitted to do
// with this activity when it meets it unfinished during recovery.
//
// Per ADR-006 the class is declared by the tool adapter, not by the call site.
// Whether booking is idempotent is a property of the booking provider, not of
// the line of workflow code that calls it.
type Class uint8

const (
	// Pure: same input produces an equivalent result, no external effect.
	// Recovery may replay freely.
	Pure Class = iota

	// ReadOnly: no external effect, but the answer may differ between calls
	// (a live quote, a search). Safe to replay; the result may change.
	ReadOnly

	// Idempotent: the external system accepts a stable key and collapses
	// repeats. Recovery retries with the same key.
	Idempotent

	// Compensatable: the effect can be undone by an explicit compensating
	// action. Recovery inspects the effect ledger, then retries or compensates.
	Compensatable

	// NonIdempotent: irreversible, with no key and no compensation. Recovery
	// must never blindly replay; it reconciles, and escalates to a human when
	// it cannot determine the outcome.
	NonIdempotent

	// NondeterministicExpensive: a model call or similar. Mechanically it is
	// memoized exactly like Pure; the difference is cost, not mechanism, which
	// is why recovery prefers the recorded output over recomputation.
	NondeterministicExpensive
)

// HasExternalEffect reports whether executing this activity can change state
// outside the runtime. It decides whether an effect-ledger row must be written
// before the call (invariant I10).
func (c Class) HasExternalEffect() bool {
	switch c {
	case Idempotent, Compensatable, NonIdempotent:
		return true
	default:
		return false
	}
}

// SafeToReplay reports whether recovery may re-execute this activity without
// first consulting the effect ledger.
func (c Class) SafeToReplay() bool {
	switch c {
	case Pure, ReadOnly:
		return true
	default:
		return false
	}
}

// PrefersRecordedOutput reports whether recovery should reuse a recorded result
// rather than recompute it, when both are available.
func (c Class) PrefersRecordedOutput() bool {
	return c == NondeterministicExpensive
}

// Narrow returns the stricter of two classes. A call site may declare an
// activity more conservatively than its adapter does, but never more
// permissively (ADR-006).
func (c Class) Narrow(other Class) Class {
	if other > c {
		return other
	}
	return c
}

func (c Class) String() string {
	switch c {
	case Pure:
		return "PURE"
	case ReadOnly:
		return "READ_ONLY"
	case Idempotent:
		return "IDEMPOTENT"
	case Compensatable:
		return "COMPENSATABLE"
	case NonIdempotent:
		return "NON_IDEMPOTENT"
	case NondeterministicExpensive:
		return "NONDETERMINISTIC_EXPENSIVE"
	default:
		return fmt.Sprintf("Class(%d)", uint8(c))
	}
}
