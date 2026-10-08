package workflow

import "testing"

// The transition tables in docs/state-machines.md, as executable tests.
//
// Illegal transitions matter more than legal ones here. A runtime that permits
// CONFIRMED -> FAILED will produce duplicate bookings, and no amount of
// testing the happy path would catch it.

func TestWorkflowTransitions(t *testing.T) {
	legal := []struct{ from, to State }{
		{Pending, Running},
		{Running, Waiting},
		{Running, Recovering},
		{Running, Completed},
		{Running, Failed},
		{Waiting, Running},
		{Recovering, Running},
		{Recovering, ManualReconciliation},
		{ManualReconciliation, Running},
		{ManualReconciliation, Failed},
	}
	for _, tc := range legal {
		if !tc.from.CanTransitionTo(tc.to) {
			t.Errorf("%s -> %s should be legal", tc.from, tc.to)
		}
	}

	illegal := []struct {
		from, to State
		why      string
	}{
		{Completed, Running, "I15: a terminal workflow stays terminal"},
		{Completed, Failed, "I15: a terminal workflow stays terminal"},
		{Failed, Running, "I15: a terminal workflow stays terminal"},
		{Failed, Completed, "I15: a terminal workflow stays terminal"},
		{Pending, Completed, "work cannot complete before it starts"},
		{Pending, Waiting, "cannot wait before running"},
		{Running, Pending, "progress does not reverse"},
		{Recovering, Completed, "recovery must resume before completing"},
	}
	for _, tc := range illegal {
		if tc.from.CanTransitionTo(tc.to) {
			t.Errorf("%s -> %s must be illegal (%s)", tc.from, tc.to, tc.why)
		}
	}
}

func TestWorkflowTerminalStates(t *testing.T) {
	for _, s := range []State{Completed, Failed} {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
		if len(workflowTransitions[s]) != 0 {
			t.Errorf("%s is terminal but has outgoing transitions", s)
		}
	}
	for _, s := range []State{Pending, Running, Waiting, Recovering, ManualReconciliation} {
		if s.IsTerminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestAttemptTransitions(t *testing.T) {
	legal := []struct{ from, to AttemptState }{
		{AttemptScheduled, AttemptStarted},
		{AttemptScheduled, AttemptAbandoned},
		{AttemptStarted, AttemptSucceeded},
		{AttemptStarted, AttemptFailedRetryable},
		{AttemptStarted, AttemptFailedTerminal},
		{AttemptStarted, AttemptAbandoned},
	}
	for _, tc := range legal {
		if !tc.from.CanTransitionTo(tc.to) {
			t.Errorf("%s -> %s should be legal", tc.from, tc.to)
		}
	}

	illegal := []struct {
		from, to AttemptState
		why      string
	}{
		{AttemptSucceeded, AttemptStarted, "I7: a succeeded attempt is never rerun in place"},
		{AttemptSucceeded, AttemptFailedRetryable, "I7: a succeeded result is never overwritten"},
		{AttemptFailedTerminal, AttemptStarted, "a retry creates a new attempt row"},
		{AttemptAbandoned, AttemptSucceeded, "an abandoned attempt never resolves itself"},
		{AttemptScheduled, AttemptSucceeded, "work cannot succeed without starting"},
	}
	for _, tc := range illegal {
		if tc.from.CanTransitionTo(tc.to) {
			t.Errorf("%s -> %s must be illegal (%s)", tc.from, tc.to, tc.why)
		}
	}
}

// TestAbandonedIsDistinctFromFailed guards the design decision that an
// abandoned attempt is not a failed one. An abandoned attempt tells us nothing
// about whether its side effect happened; collapsing the two would discard the
// information the effect ledger exists to hold.
func TestAbandonedIsDistinctFromFailed(t *testing.T) {
	if AttemptAbandoned == AttemptFailedRetryable || AttemptAbandoned == AttemptFailedTerminal {
		t.Fatal("ABANDONED must be a distinct state from the FAILED states")
	}
	if !AttemptAbandoned.IsTerminal() {
		t.Error("ABANDONED should be terminal for the attempt")
	}
}

func TestEffectTransitions(t *testing.T) {
	legal := []struct{ from, to EffectState }{
		{EffectPrepared, EffectConfirmed},
		{EffectPrepared, EffectFailed},
		{EffectPrepared, EffectUnknown},
		{EffectUnknown, EffectConfirmed},
		{EffectUnknown, EffectFailed},
		{EffectUnknown, EffectCompensated},
		{EffectConfirmed, EffectCompensated},
	}
	for _, tc := range legal {
		if !tc.from.CanTransitionTo(tc.to) {
			t.Errorf("%s -> %s should be legal", tc.from, tc.to)
		}
	}
}

// TestForbiddenEffectTransitions is the most important test in this file.
//
// CONFIRMED -> FAILED is the transition that creates duplicate bookings: a
// runtime that demotes a known-good effect because a later response was lost
// will reissue it. The others are the same class of mistake.
func TestForbiddenEffectTransitions(t *testing.T) {
	forbidden := []struct {
		from, to EffectState
		why      string
	}{
		{EffectConfirmed, EffectPrepared, "an effect cannot un-happen"},
		{EffectConfirmed, EffectUnknown, "once known, never un-known"},
		{EffectConfirmed, EffectFailed, "THE duplicate-booking bug"},
		{EffectFailed, EffectConfirmed, "a definite rejection does not later succeed"},
		{EffectCompensated, EffectConfirmed, "a compensated effect does not revive"},
		{EffectUnknown, EffectPrepared, "PREPARED is only ever an initial state"},
		{EffectFailed, EffectUnknown, "a known outcome does not become unknown"},
	}
	for _, tc := range forbidden {
		if tc.from.CanTransitionTo(tc.to) {
			t.Errorf("%s -> %s must be forbidden (%s)", tc.from, tc.to, tc.why)
		}
	}
}

// TestUnknownIsNotKnownTerminal encodes invariant I12. UNKNOWN is terminal for
// the state machine but is not a known outcome, so a workflow holding one must
// not be allowed to complete.
func TestUnknownIsNotKnownTerminal(t *testing.T) {
	if EffectUnknown.IsKnownTerminal() {
		t.Fatal("UNKNOWN must not count as a known terminal state (invariant I12)")
	}
	for _, s := range []EffectState{EffectConfirmed, EffectFailed, EffectCompensated} {
		if !s.IsKnownTerminal() {
			t.Errorf("%s should be a known terminal state", s)
		}
	}
	if EffectPrepared.IsKnownTerminal() {
		t.Error("PREPARED is not terminal at all")
	}
}

// TestNoTransitionIntoPrepared: PREPARED is an initial state only. Anything
// reaching it later would mean an effect was reset and could be reissued.
func TestNoTransitionIntoPrepared(t *testing.T) {
	for from, targets := range effectTransitions {
		for _, to := range targets {
			if to == EffectPrepared {
				t.Errorf("%s -> PREPARED exists; PREPARED must be initial only", from)
			}
		}
	}
}
