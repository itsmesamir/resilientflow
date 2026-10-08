package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func newCtx(t *testing.T, j Journal) *Context {
	t.Helper()
	return NewContext(context.Background(), "run-A", j, 42)
}

func echo(calls *int, name string, class Class) Activity[string, string] {
	return Activity[string, string]{
		Name:  name,
		Class: class,
		Fn: func(_ context.Context, _ Call, in string) (string, error) {
			*calls++
			return "out:" + in, nil
		},
	}
}

// ---------------------------------------------------------------------------
// ADR-003: idempotency key derivation
// ---------------------------------------------------------------------------

func TestDeriveKeyIsDeterministic(t *testing.T) {
	first := DeriveKey("run-A", 7)
	for i := 0; i < 100; i++ {
		if got := DeriveKey("run-A", 7); got != first {
			t.Fatalf("key is not stable: %s then %s", first, got)
		}
	}
	if first == "" {
		t.Fatal("key must not be empty")
	}
}

func TestDeriveKeyVariesByRunAndPosition(t *testing.T) {
	seen := map[string]string{}
	for _, run := range []string{"run-A", "run-B"} {
		for seq := 1; seq <= 20; seq++ {
			k := DeriveKey(run, seq)
			if where, dup := seen[k]; dup {
				t.Fatalf("key collision between %s/%d and %s", run, seq, where)
			}
			seen[k] = run + "/" + string(rune('0'+seq%10))
		}
	}
}

// TestKeyIsStableAcrossAttempts is the executable form of invariant I9, and the
// single most important property in the project.
//
// An activity fails on its first execution and succeeds when the run is
// resumed. Every request that reached the outside world must have carried the
// same key, or a duplicate effect follows immediately.
func TestKeyIsStableAcrossAttempts(t *testing.T) {
	j := NewMemoryJournal()

	failing := Activity[string, string]{
		Name:  "charge_card",
		Class: Idempotent,
		Fn: func(_ context.Context, _ Call, _ string) (string, error) {
			return "", errors.New("timeout")
		},
	}
	succeeding := Activity[string, string]{
		Name:  "charge_card",
		Class: Idempotent,
		Fn: func(_ context.Context, _ Call, in string) (string, error) {
			return "ok:" + in, nil
		},
	}

	if _, err := Execute(newCtx(t, j), failing, "input"); err == nil {
		t.Fatal("expected the first attempt to fail")
	}
	if _, err := Execute(newCtx(t, j), succeeding, "input"); err != nil {
		t.Fatalf("resumed attempt failed: %v", err)
	}

	keys := map[string]bool{}
	for _, r := range j.All() {
		if r.IdempotencyKey != "" {
			keys[r.IdempotencyKey] = true
		}
	}
	if len(keys) != 1 {
		t.Fatalf("invariant I9 violated: %d distinct keys for one logical activity, want 1", len(keys))
	}
	if v := CheckInvariants(j); len(v) != 0 {
		t.Fatalf("invariant violations: %v", v)
	}
}

// TestKeyIgnoresInput documents the trade-off ADR-003 accepts deliberately.
//
// A re-synthesized, genuinely different request arrives under the SAME key, so
// the external system returns the original effect. No duplicate — but the
// effect may no longer match current intent. That is the intent-divergence
// failure the design chooses, and it is detectable because the input hash is
// recorded separately.
func TestKeyIgnoresInput(t *testing.T) {
	a := DeriveKey("run-A", 3)
	b := DeriveKey("run-A", 3)
	if a != b {
		t.Fatal("key must not depend on anything beyond run and position")
	}
}

// ---------------------------------------------------------------------------
// Replay with memoization (ADR-001)
// ---------------------------------------------------------------------------

func TestExecuteRunsOnFreshJournal(t *testing.T) {
	j := NewMemoryJournal()
	calls := 0

	got, err := Execute(newCtx(t, j), echo(&calls, "step", Pure), "x")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got != "out:x" {
		t.Fatalf("got %q, want %q", got, "out:x")
	}
	if calls != 1 {
		t.Fatalf("activity ran %d times, want 1", calls)
	}
}

// TestReplayReusesRecordedResult is the mechanism the whole runtime is built
// on: on replay the activity must not run again, and the result must match.
func TestReplayReusesRecordedResult(t *testing.T) {
	j := NewMemoryJournal()
	calls := 0

	first, err := Execute(newCtx(t, j), echo(&calls, "step", Pure), "x")
	if err != nil {
		t.Fatalf("first run: %v", err)
	}

	second, err := Execute(newCtx(t, j), echo(&calls, "step", Pure), "x")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if calls != 1 {
		t.Fatalf("activity ran %d times across original and replay, want 1", calls)
	}
	if first != second {
		t.Fatalf("replay returned %q, original returned %q", second, first)
	}
}

func TestReplayDetectsInputMismatch(t *testing.T) {
	j := NewMemoryJournal()
	calls := 0

	if _, err := Execute(newCtx(t, j), echo(&calls, "step", Pure), "original"); err != nil {
		t.Fatalf("first run: %v", err)
	}

	_, err := Execute(newCtx(t, j), echo(&calls, "step", Pure), "DIFFERENT")
	if !errors.Is(err, ErrInputMismatch) {
		t.Fatalf("got %v, want ErrInputMismatch", err)
	}
	if calls != 1 {
		t.Fatal("a mismatched replay must not execute the activity")
	}
}

func TestReplayDetectsNondeterminism(t *testing.T) {
	j := NewMemoryJournal()
	calls := 0

	if _, err := Execute(newCtx(t, j), echo(&calls, "step_one", Pure), "x"); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// The workflow called a different activity at the same position: the
	// determinism contract of ADR-001 was broken.
	_, err := Execute(newCtx(t, j), echo(&calls, "step_two", Pure), "x")
	if !errors.Is(err, ErrNondeterministic) {
		t.Fatalf("got %v, want ErrNondeterministic", err)
	}
}

// ---------------------------------------------------------------------------
// Invariant I10: PREPARED precedes the request
// ---------------------------------------------------------------------------

func TestPreparedIsCommittedBeforeExternalCall(t *testing.T) {
	j := NewMemoryJournal()
	var sawPrepared bool
	var keyAtCallTime string

	act := Activity[string, string]{
		Name:  "create_booking",
		Class: Idempotent,
		Fn: func(_ context.Context, call Call, _ string) (string, error) {
			// At the moment the external call would be made, the ledger row
			// must already exist and must already carry the key.
			rec, ok := j.Latest(call.Seq)
			sawPrepared = ok && rec.Effect == EffectPrepared
			keyAtCallTime = rec.IdempotencyKey
			return "booked", nil
		},
	}

	if _, err := Execute(newCtx(t, j), act, "x"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !sawPrepared {
		t.Fatal("invariant I10 violated: no PREPARED record existed when the external call was made")
	}
	if keyAtCallTime != DeriveKey("run-A", 1) {
		t.Fatal("the PREPARED record must carry the idempotency key")
	}
}

func TestNoLedgerRowForPureActivity(t *testing.T) {
	j := NewMemoryJournal()
	calls := 0

	if _, err := Execute(newCtx(t, j), echo(&calls, "rank", Pure), "x"); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, r := range j.All() {
		if r.Effect != "" {
			t.Fatalf("pure activity produced an effect record: %+v", r)
		}
	}
}

// TestFailedExternalCallBecomesUnknown: a failure with no definite answer must
// be recorded as UNKNOWN, never as FAILED. Demoting it to FAILED is what
// produces the duplicate booking.
func TestFailedExternalCallBecomesUnknown(t *testing.T) {
	j := NewMemoryJournal()

	act := Activity[string, string]{
		Name:  "create_booking",
		Class: Idempotent,
		Fn: func(_ context.Context, _ Call, _ string) (string, error) {
			return "", errors.New("connection reset")
		},
	}

	if _, err := Execute(newCtx(t, j), act, "x"); err == nil {
		t.Fatal("expected failure")
	}

	effects := Effects(j)
	if got := effects[1]; got != EffectUnknown {
		t.Fatalf("effect is %s, want UNKNOWN", got)
	}
	if effects[1].IsKnownTerminal() {
		t.Fatal("UNKNOWN must not be treated as a known outcome")
	}
}

// definiteErr is a conclusive refusal: the service considered the request and
// created nothing.
type definiteErr struct{}

func (definiteErr) Error() string         { return "rejected" }
func (definiteErr) DefiniteFailure() bool { return true }

// TestDefiniteFailureIsRecordedAsFailed: when the service tells us it did
// nothing, believing it is correct and cheaper than reconciling.
func TestDefiniteFailureIsRecordedAsFailed(t *testing.T) {
	j := NewMemoryJournal()

	act := Activity[string, string]{
		Name:  "create_booking",
		Class: Idempotent,
		Fn: func(_ context.Context, _ Call, _ string) (string, error) {
			return "", definiteErr{}
		},
	}

	if _, err := Execute(newCtx(t, j), act, "x"); err == nil {
		t.Fatal("expected failure")
	}

	if got := Effects(j)[1]; got != EffectFailed {
		t.Fatalf("effect is %s, want FAILED — the service said it created nothing", got)
	}
}

// TestUnrecognisedErrorDefaultsToUnknown guards the conservative default. An
// error we cannot interpret must never be read as proof that nothing happened.
func TestUnrecognisedErrorDefaultsToUnknown(t *testing.T) {
	j := NewMemoryJournal()

	act := Activity[string, string]{
		Name:  "create_booking",
		Class: Idempotent,
		Fn: func(_ context.Context, _ Call, _ string) (string, error) {
			return "", errors.New("i/o timeout")
		},
	}

	if _, err := Execute(newCtx(t, j), act, "x"); err == nil {
		t.Fatal("expected failure")
	}

	if got := Effects(j)[1]; got != EffectUnknown {
		t.Fatalf("effect is %s, want UNKNOWN", got)
	}
}

func TestIsDefiniteFailureUnwrapsWrappedErrors(t *testing.T) {
	wrapped := fmt.Errorf("create_booking: %w", definiteErr{})
	if !IsDefiniteFailure(wrapped) {
		t.Error("definiteness must survive error wrapping")
	}
	if IsDefiniteFailure(errors.New("plain")) {
		t.Error("a plain error must not be treated as definite")
	}
	if IsDefiniteFailure(nil) {
		t.Error("nil is not a definite failure")
	}
}

// ---------------------------------------------------------------------------
// Determinism helpers
// ---------------------------------------------------------------------------

func TestDeterminismHelpersReproduceOnReplay(t *testing.T) {
	j := NewMemoryJournal()

	c1 := newCtx(t, j)
	t1, r1, u1 := c1.Now(), c1.Random(), c1.UUID()

	c2 := newCtx(t, j)
	t2, r2, u2 := c2.Now(), c2.Random(), c2.UUID()

	if !t1.Equal(t2) {
		t.Errorf("Now() diverged on replay: %v then %v", t1, t2)
	}
	if r1 != r2 {
		t.Errorf("Random() diverged on replay: %d then %d", r1, r2)
	}
	if u1 != u2 {
		t.Errorf("UUID() diverged on replay: %s then %s", u1, u2)
	}
}

func TestSequenceNumbersAreGapless(t *testing.T) {
	j := NewMemoryJournal()
	c := newCtx(t, j)
	calls := 0

	for i := 0; i < 5; i++ {
		if _, err := Execute(c, echo(&calls, "step", Pure), "x"); err != nil {
			t.Fatalf("execute %d: %v", i, err)
		}
		c.Now()
	}

	if v := CheckInvariants(j); len(v) != 0 {
		t.Fatalf("invariant violations: %v", v)
	}
}
