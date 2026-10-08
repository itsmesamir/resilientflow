package workflow

import (
	"context"
	"fmt"
)

// Definition is a workflow: a name and a function over a Context.
//
// The function must obey the determinism contract (ADR-001): no direct clock
// reads, no randomness, no map-iteration-order dependence, no I/O outside
// Execute. Everything nondeterministic goes through the Context.
type Definition[I, O any] struct {
	Name string
	Fn   func(*Context, I) (O, error)
}

// Result is the outcome of one run.
type Result[O any] struct {
	Output O
	State  State
	Err    error

	// Executed counts activities that actually ran, as opposed to being served
	// from the journal. On a fresh run it equals the activity count; on a
	// replay it is the recomputation cost, which is one of the headline metrics
	// the benchmark reports.
	Executed int

	// Replayed counts activities served from recorded results.
	Replayed int
}

// Run executes a workflow to completion or failure.
//
// Phase 1 is a single process with an in-memory journal and no recovery: if the
// process dies, everything is lost. That is Baseline A, and demonstrating it is
// the point of this milestone.
func Run[I, O any](
	ctx context.Context,
	def Definition[I, O],
	runID string,
	input I,
	j Journal,
	seed int64,
) (Result[O], *Context) {
	wctx := NewContext(ctx, runID, j, seed)
	return runWith(def, input, wctx), wctx
}

// RunWith executes a workflow using a caller-supplied Context, so that tests
// and the fault harness can install a BeforeActivity hook.
func RunWith[I, O any](def Definition[I, O], input I, wctx *Context) Result[O] {
	return runWith(def, input, wctx)
}

func runWith[I, O any](def Definition[I, O], input I, wctx *Context) Result[O] {
	before := wctx.journal.Len()

	out, err := def.Fn(wctx, input)

	executed, replayed := accounting(wctx.journal, before)

	// Invariant I12: an effect that is not in a terminal, KNOWN state means we
	// do not know what happened in the outside world.
	//
	// This check comes BEFORE the error check, and the ordering is the whole
	// point. The workflow function will have returned an error — the external
	// call failed, from its point of view. Reporting FAILED would assert that
	// the effect did not happen, which we have no basis for. Reporting
	// MANUAL_RECONCILIATION says what is actually true: something is out there
	// and we cannot account for it.
	//
	// Demoting UNKNOWN to FAILED on the strength of a lost response is the bug
	// that produces duplicate bookings (docs/failure-model.md §3).
	if seq, st, found := firstUnresolved(wctx.journal); found {
		return Result[O]{
			State:    ManualReconciliation,
			Err:      fmt.Errorf("%w: effect at seq %d is %s", ErrEffectUnresolved, seq, st),
			Executed: executed,
			Replayed: replayed,
		}
	}

	if err != nil {
		return Result[O]{State: Failed, Err: err, Executed: executed, Replayed: replayed}
	}

	return Result[O]{Output: out, State: Completed, Executed: executed, Replayed: replayed}
}

// firstUnresolved returns the lowest-numbered effect that is not in a known
// terminal state.
//
// It walks the journal in order rather than ranging over the Effects map, so
// that a workflow with several unresolved effects reports the same one on every
// run. Map iteration order would make the result nondeterministic, and a
// nondeterministic error message is a bad thing to debug a crash with.
func firstUnresolved(j Journal) (int, EffectState, bool) {
	latest := map[int]EffectState{}
	var order []int
	for _, r := range j.All() {
		if r.Effect == "" {
			continue
		}
		if _, seen := latest[r.Seq]; !seen {
			order = append(order, r.Seq)
		}
		latest[r.Seq] = r.Effect
	}
	for _, seq := range order {
		if st := latest[seq]; !st.IsKnownTerminal() {
			return seq, st, true
		}
	}
	return 0, "", false
}

// accounting separates work actually performed in this run from work served out
// of the journal. Records appended during the run mean execution happened;
// activity sequence numbers already present beforehand were replayed.
func accounting(j Journal, before int) (executed, replayed int) {
	all := j.All()

	seen := map[int]bool{}
	for _, r := range all[:before] {
		if r.Kind == KindActivity && r.Status == AttemptSucceeded {
			seen[r.Seq] = true
		}
	}
	replayed = len(seen)

	done := map[int]bool{}
	for _, r := range all[before:] {
		if r.Kind == KindActivity && r.Status != AttemptStarted && !seen[r.Seq] {
			done[r.Seq] = true
		}
	}
	executed = len(done)
	return executed, replayed
}

// ---------------------------------------------------------------------------
// Invariant checker
// ---------------------------------------------------------------------------

// Violation is an invariant breach found in a run's journal.
type Violation struct {
	Invariant string
	Detail    string
}

func (v Violation) String() string { return v.Invariant + ": " + v.Detail }

// CheckInvariants verifies the journal against the invariants that are testable
// from a single run's history. It runs after every benchmark run from Phase 3
// onward; a run with any violation is excluded from results AND investigated,
// never quietly dropped. The count of exclusions is itself reported.
func CheckInvariants(j Journal) []Violation {
	var out []Violation
	all := j.All()

	// I2: sequence numbers are unique and gapless, 1..n.
	maxSeq := 0
	present := map[int]bool{}
	for _, r := range all {
		present[r.Seq] = true
		if r.Seq > maxSeq {
			maxSeq = r.Seq
		}
		if r.Seq < 1 {
			out = append(out, Violation{"I2", fmt.Sprintf("sequence number %d is below 1", r.Seq)})
		}
	}
	for s := 1; s <= maxSeq; s++ {
		if !present[s] {
			out = append(out, Violation{"I2", fmt.Sprintf("gap at sequence number %d", s)})
		}
	}

	// I11: effect states never regress along a forbidden edge.
	last := map[int]EffectState{}
	for _, r := range all {
		if r.Effect == "" {
			continue
		}
		prev, ok := last[r.Seq]
		if ok && prev != r.Effect && !prev.CanTransitionTo(r.Effect) {
			out = append(out, Violation{"I11", fmt.Sprintf(
				"seq %d: forbidden effect transition %s -> %s", r.Seq, prev, r.Effect)})
		}
		last[r.Seq] = r.Effect
	}

	// I7: a succeeded attempt's output is never rewritten.
	succeeded := map[int]string{}
	for _, r := range all {
		if r.Kind != KindActivity || r.Status != AttemptSucceeded {
			continue
		}
		if prior, ok := succeeded[r.Seq]; ok && prior != string(r.Output) {
			out = append(out, Violation{"I7", fmt.Sprintf(
				"seq %d: output of a succeeded attempt was rewritten", r.Seq)})
		}
		succeeded[r.Seq] = string(r.Output)
	}

	// I9: one logical activity never uses more than one idempotency key.
	keys := map[int]string{}
	for _, r := range all {
		if r.IdempotencyKey == "" {
			continue
		}
		if prior, ok := keys[r.Seq]; ok && prior != r.IdempotencyKey {
			out = append(out, Violation{"I9", fmt.Sprintf(
				"seq %d: two different idempotency keys (%s, %s)", r.Seq, prior, r.IdempotencyKey)})
		}
		keys[r.Seq] = r.IdempotencyKey
	}

	return out
}
