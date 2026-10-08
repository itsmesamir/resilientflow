package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"time"
)

// Call carries the per-call identity the runtime derives for an activity. It is
// passed to the activity function so that tool adapters can send the
// idempotency key to the external system.
//
// Making this explicit rather than hiding it in a context value is deliberate:
// the key derivation is the most consequential decision in the project, and the
// code should show it.
type Call struct {
	// Seq is the activity's position in the deterministic execution order.
	Seq int

	// IdempotencyKey is derived from the run ID and Seq. It is stable across
	// every attempt at this logical operation, including attempts made by
	// different workers that cannot communicate.
	IdempotencyKey string

	// Attempt counts executions of this call within the current run, starting
	// at 1. It deliberately does NOT feed the key.
	Attempt int
}

// Activity is a unit of work with declared replay semantics.
//
// Class is declared here, by the adapter that owns the activity, not by the
// workflow that calls it (ADR-006).
type Activity[I, O any] struct {
	Name  string
	Class Class
	Fn    func(ctx context.Context, call Call, input I) (O, error)
}

// Context carries the deterministic execution state of one workflow run.
//
// Every source of nondeterminism a workflow might reach for — the clock,
// randomness, UUIDs — is routed through this type and journaled, so that a
// replay reproduces the original run rather than making fresh decisions. This
// is the determinism contract that ADR-001 requires.
//
// A Context is not safe for concurrent use. Workflow code is sequential by
// design: the sequence counter that assigns each call its position, and
// therefore its idempotency key, is only deterministic if calls happen in one
// order. Parallel activities will need an explicit construct that assigns
// positions deterministically rather than by whichever goroutine arrives first.
type Context struct {
	ctx     context.Context
	runID   string
	journal Journal
	rng     *rand.Rand
	clock   func() time.Time

	seq      int
	attempts map[int]int

	// BeforeActivity, when set, runs immediately before each activity executes.
	// The fault harness uses it to crash the process at a chosen point; it is
	// the Phase 1 stand-in for faultctl.
	BeforeActivity func(name string, seq int)
}

// NewContext creates an execution context. seed makes the run reproducible:
// the same seed produces the same randomness, which is a precondition for a
// benchmark whose results mean anything.
func NewContext(ctx context.Context, runID string, j Journal, seed int64) *Context {
	return &Context{
		ctx:      ctx,
		runID:    runID,
		journal:  j,
		rng:      rand.New(rand.NewSource(seed)),
		clock:    time.Now,
		attempts: map[int]int{},
	}
}

// RunID returns the workflow run's identity.
func (c *Context) RunID() string { return c.runID }

// Journal exposes the run's history, for tests and the invariant checker.
func (c *Context) Journal() Journal { return c.journal }

// Err returns the underlying context's error, so workflow code can observe
// cancellation without reaching for the raw context.
func (c *Context) Err() error { return c.ctx.Err() }

func (c *Context) next() int {
	c.seq++
	return c.seq
}

// DeriveKey computes an idempotency key from the run's identity and the
// activity's position in the execution order.
//
// This is ADR-003, and it is the decision the project turns on. The key
// deliberately excludes:
//
//   - the attempt number — every retry would otherwise be a new operation,
//     which guarantees duplicates;
//   - any timestamp — two workers would compute different keys;
//   - the input hash — if upstream nondeterminism changes the input, a
//     content-derived key changes with it, and the mechanism meant to prevent
//     duplication is defeated by the nondeterminism it exists to tolerate.
//
// That last exclusion is the one with external evidence behind it. PostHog's
// April 2026 postmortem (a queue boundary rewrote their dedup key, silently
// dropping 11,920 legitimate invocations) and ACRFence (agents re-synthesize a
// subtly different request after restore, which the server sees as new) report
// the same failure from opposite directions. See docs/related-work.md §4.
//
// The trade-off this accepts is intent divergence: a genuinely different
// request arrives under the same key, so the external system returns the
// original effect. No duplicate, but the effect may no longer match what the
// workflow now wants. InputHash is recorded separately so that divergence is
// detectable (ErrInputMismatch) rather than silent.
func DeriveKey(runID string, seq int) string {
	h := sha256.New()
	h.Write([]byte(runID))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", seq)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func hashJSON(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Execute runs an activity, or returns its recorded result if this run has
// already completed it.
//
// This is the heart of the replay-with-memoization model (ADR-001). On a fresh
// run every call executes. On a replay, each call consults the journal first,
// and the decision of whether to re-execute or reuse is exactly where recovery
// policy lives — which is why the research question sits here rather than being
// bolted on elsewhere.
func Execute[I, O any](c *Context, a Activity[I, O], input I) (O, error) {
	var zero O

	seq := c.next()
	key := DeriveKey(c.runID, seq)

	rawInput, err := json.Marshal(input)
	if err != nil {
		return zero, fmt.Errorf("marshal input for %q: %w", a.Name, err)
	}
	inputHash := hashJSON(rawInput)

	// Replay path: consult the journal before doing anything.
	prior, hasPrior := c.journal.Latest(seq)
	if hasPrior && prior.Kind == KindActivity {
		if prior.Name != a.Name {
			return zero, fmt.Errorf("%w: seq %d was recorded as %q but replay called %q",
				ErrNondeterministic, seq, prior.Name, a.Name)
		}
		if prior.Status == AttemptSucceeded {
			// Divergence is reported, but the key is unchanged, so no duplicate
			// effect can result from it.
			if prior.InputHash != inputHash {
				return zero, fmt.Errorf("%w: activity %q at seq %d (recorded %s, replayed %s)",
					ErrInputMismatch, a.Name, seq, prior.InputHash, inputHash)
			}
			var out O
			if err := json.Unmarshal(prior.Output, &out); err != nil {
				return zero, fmt.Errorf("unmarshal recorded output for %q: %w", a.Name, err)
			}
			return out, nil
		}
	}

	c.attempts[seq]++
	call := Call{Seq: seq, IdempotencyKey: key, Attempt: c.attempts[seq]}

	base := Record{
		Seq:            seq,
		Kind:           KindActivity,
		Name:           a.Name,
		Class:          a.Class,
		InputHash:      inputHash,
		IdempotencyKey: key,
	}

	// Invariant I10: for anything with an external effect, the key is committed
	// BEFORE the request is sent. A crash between send and commit must leave a
	// record that a request was made, or recovery has no key to retry with.
	//
	// PREPARED is written exactly once, on the first attempt. A retry of an
	// effect already in UNKNOWN must not write it again: the key was committed
	// by the first attempt and has not changed, and UNKNOWN -> PREPARED is a
	// forbidden transition (invariant I11) because it would suggest the effect
	// had been reset and could be reissued under fresh terms.
	if a.Class.HasExternalEffect() && !(hasPrior && prior.Effect != "") {
		prepared := base
		prepared.Status = AttemptStarted
		prepared.Effect = EffectPrepared
		if err := c.journal.Append(prepared); err != nil {
			return zero, fmt.Errorf("journal prepared record for %q: %w", a.Name, err)
		}
	}

	if c.BeforeActivity != nil {
		c.BeforeActivity(a.Name, seq)
	}

	out, runErr := a.Fn(c.ctx, call, input)

	if runErr != nil {
		rec := base
		rec.Status = AttemptFailedRetryable
		rec.Err = runErr.Error()
		if a.Class.HasExternalEffect() {
			// A conclusive rejection means the service considered the request
			// and created nothing. Anything else — a timeout, a reset, a lost
			// response — leaves the outcome genuinely unknown, and we must not
			// pretend otherwise just because an error was returned.
			if IsDefiniteFailure(runErr) {
				rec.Effect = EffectFailed
			} else {
				rec.Effect = EffectUnknown
			}
		}
		if appendErr := c.journal.Append(rec); appendErr != nil {
			return zero, fmt.Errorf("journal failure record for %q: %w", a.Name, appendErr)
		}
		return zero, runErr
	}

	rawOutput, err := json.Marshal(out)
	if err != nil {
		return zero, fmt.Errorf("marshal output for %q: %w", a.Name, err)
	}

	rec := base
	rec.Status = AttemptSucceeded
	rec.Output = rawOutput
	if a.Class.HasExternalEffect() {
		rec.Effect = EffectConfirmed
	}
	if err := c.journal.Append(rec); err != nil {
		return zero, fmt.Errorf("journal success record for %q: %w", a.Name, err)
	}

	return out, nil
}

// ---------------------------------------------------------------------------
// Determinism helpers
//
// Workflow code must never call time.Now, rand, or generate a UUID directly.
// Those make replay diverge from the recorded history. These three route the
// value through the journal so a replay reproduces it.
// ---------------------------------------------------------------------------

// Now returns the workflow's notion of the current time, recorded on first call
// and reproduced on replay.
func (c *Context) Now() time.Time {
	seq := c.next()
	if rec, ok := c.journal.Latest(seq); ok && rec.Kind == KindNow {
		var t time.Time
		if err := json.Unmarshal(rec.Output, &t); err == nil {
			return t
		}
	}
	t := c.clock().UTC()
	raw, _ := json.Marshal(t)
	_ = c.journal.Append(Record{Seq: seq, Kind: KindNow, Output: raw, Status: AttemptSucceeded})
	return t
}

// Random returns a deterministic pseudo-random int64, recorded and reproduced.
func (c *Context) Random() int64 {
	seq := c.next()
	if rec, ok := c.journal.Latest(seq); ok && rec.Kind == KindRandom {
		var v int64
		if err := json.Unmarshal(rec.Output, &v); err == nil {
			return v
		}
	}
	v := c.rng.Int63()
	raw, _ := json.Marshal(v)
	_ = c.journal.Append(Record{Seq: seq, Kind: KindRandom, Output: raw, Status: AttemptSucceeded})
	return v
}

// UUID returns a deterministic identifier, recorded and reproduced.
func (c *Context) UUID() string {
	seq := c.next()
	if rec, ok := c.journal.Latest(seq); ok && rec.Kind == KindUUID {
		var s string
		if err := json.Unmarshal(rec.Output, &s); err == nil {
			return s
		}
	}
	var b [16]byte
	for i := range b {
		b[i] = byte(c.rng.Intn(256))
	}
	s := hex.EncodeToString(b[:])
	raw, _ := json.Marshal(s)
	_ = c.journal.Append(Record{Seq: seq, Kind: KindUUID, Output: raw, Status: AttemptSucceeded})
	return s
}
