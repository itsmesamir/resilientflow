package workflow

// The three state machines specified in docs/state-machines.md.
//
// The transition tables below are the executable form of that document. Any
// transition not listed is illegal and is failure F6.4: it indicates a bug in
// the runtime, not an environmental fault, and must fail loudly rather than be
// silently ignored.

// ---------------------------------------------------------------------------
// Workflow execution
// ---------------------------------------------------------------------------

type State string

const (
	Pending              State = "PENDING"
	Running              State = "RUNNING"
	Waiting              State = "WAITING"
	Recovering           State = "RECOVERING"
	Completed            State = "COMPLETED"
	Failed               State = "FAILED"
	ManualReconciliation State = "MANUAL_RECONCILIATION"
)

var workflowTransitions = map[State][]State{
	Pending:              {Running},
	Running:              {Waiting, Recovering, Completed, Failed},
	Waiting:              {Running, Recovering, Failed},
	Recovering:           {Running, ManualReconciliation, Failed},
	ManualReconciliation: {Running, Failed},

	// Terminal. Invariant I15: no transition leaves these.
	Completed: {},
	Failed:    {},
}

func (s State) CanTransitionTo(next State) bool {
	for _, allowed := range workflowTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// IsTerminal reports whether the workflow has reached a final state.
func (s State) IsTerminal() bool { return s == Completed || s == Failed }

// ---------------------------------------------------------------------------
// Activity attempt
// ---------------------------------------------------------------------------

type AttemptState string

const (
	AttemptScheduled       AttemptState = "SCHEDULED"
	AttemptStarted         AttemptState = "STARTED"
	AttemptSucceeded       AttemptState = "SUCCEEDED"
	AttemptFailedRetryable AttemptState = "FAILED_RETRYABLE"
	AttemptFailedTerminal  AttemptState = "FAILED_TERMINAL"

	// AttemptAbandoned means the owning worker vanished mid-flight. It is
	// deliberately distinct from FAILED: an abandoned attempt tells us nothing
	// about whether its side effect happened, and collapsing the two would
	// discard exactly the information the effect ledger needs.
	AttemptAbandoned AttemptState = "ABANDONED"
)

var attemptTransitions = map[AttemptState][]AttemptState{
	AttemptScheduled: {AttemptStarted, AttemptAbandoned},
	AttemptStarted: {
		AttemptSucceeded,
		AttemptFailedRetryable,
		AttemptFailedTerminal,
		AttemptAbandoned,
	},

	// Terminal. Invariant I7: a succeeded attempt is never rewritten; a retry
	// inserts a new attempt row rather than mutating this one.
	AttemptSucceeded:       {},
	AttemptFailedRetryable: {},
	AttemptFailedTerminal:  {},
	AttemptAbandoned:       {},
}

func (s AttemptState) CanTransitionTo(next AttemptState) bool {
	for _, allowed := range attemptTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

func (s AttemptState) IsTerminal() bool {
	return len(attemptTransitions[s]) == 0
}

// ---------------------------------------------------------------------------
// Effect ledger
// ---------------------------------------------------------------------------

type EffectState string

const (
	// EffectPrepared is written, and committed, BEFORE the external request is
	// sent (invariant I10). Writing it afterwards would leave a crash between
	// send and commit with no record that a request was ever made, so recovery
	// would have no key to retry with — which is the duplicate booking.
	EffectPrepared EffectState = "PREPARED"

	EffectConfirmed EffectState = "CONFIRMED"
	EffectFailed    EffectState = "FAILED"

	// EffectUnknown: we sent the request and do not know whether the effect
	// happened. The runtime must never convert this to FAILED merely because a
	// response was lost. See docs/failure-model.md §3.
	EffectUnknown EffectState = "UNKNOWN"

	EffectCompensated EffectState = "COMPENSATED"
)

var effectTransitions = map[EffectState][]EffectState{
	EffectPrepared: {EffectConfirmed, EffectFailed, EffectUnknown},
	EffectUnknown:  {EffectConfirmed, EffectFailed, EffectCompensated},
	EffectConfirmed: {
		// Only via an explicit compensating action (saga rollback).
		EffectCompensated,
	},

	EffectFailed:      {},
	EffectCompensated: {},
}

func (s EffectState) CanTransitionTo(next EffectState) bool {
	for _, allowed := range effectTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// IsKnownTerminal reports whether the effect has reached a terminal state whose
// outcome we actually know.
//
// UNKNOWN is terminal for the state machine but NOT known, which is why it is
// excluded here. Invariant I12: a workflow may only complete when every
// required effect is in a terminal, known state. A workflow holding an
// unresolved effect goes to MANUAL_RECONCILIATION, never to COMPLETED.
func (s EffectState) IsKnownTerminal() bool {
	switch s {
	case EffectConfirmed, EffectFailed, EffectCompensated:
		return true
	default:
		return false
	}
}
