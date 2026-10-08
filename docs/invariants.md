# Invariants

Each invariant is written so it can become an executable check. The `Enforced
by` column names the mechanism that makes a violation impossible, and the
`Checked by` column names the test that proves it.

An invariant with no enforcement mechanism is a wish, not an invariant.

---

## Event log

### I1 — The event log is append-only

> Once an event is committed it is never modified or deleted.

**Enforced by:** no `UPDATE` or `DELETE` statement against `workflow_events`
exists anywhere in the codebase; a database trigger rejects both.
**Checked by:** a test that attempts an update and expects an error, plus a grep
in CI for `UPDATE workflow_events`.

### I2 — Sequence numbers are unique and gapless per workflow

> For a workflow with n events, the sequence numbers are exactly 1..n.

**Enforced by:** `UNIQUE (workflow_id, sequence_no)`, and the next value is
computed inside the same transaction that inserts.
**Checked by:** a property test that concurrently appends from several
goroutines and then asserts the set of sequence numbers.

Unique alone would permit gaps. Gaplessness is what lets recovery know it has
seen the whole history, so it is worth the extra cost of serialising appends per
workflow.

### I3 — The log is the source of truth

> Every row in `activity_executions` and `effect_ledger` is derivable from the
> event log.

**Enforced by:** projections are written in the same transaction as the event
that produced them.
**Checked by:** a test that rebuilds both tables from the log and compares them
byte for byte with the live tables. This also becomes a periodic consistency
check in the benchmark harness.

---

## Ownership

### I4 — A write requires the current epoch

> A worker may mutate workflow state only while holding the workflow's current
> fencing epoch.

**Enforced by:** every mutating statement carries `AND owner_epoch = $n`, and
the caller checks the rows-affected count.
**Checked by:** the stale-worker test (F5.1). Pause worker A, let B take over,
resume A, assert A's write affects zero rows and that A then halts.

### I5 — Epochs increase monotonically

> A workflow's epoch never decreases, and each takeover increments it by one.

**Enforced by:** the epoch is only ever set by `SET owner_epoch = owner_epoch +
1` inside a transaction that holds a row lock.
**Checked by:** a property test running concurrent takeovers, asserting the
observed epoch sequence has no repeats and no decreases.

### I6 — One owner at a time

> At most one worker holds a valid lease on a workflow at any instant.

**Enforced by:** the lease row is unique per workflow, and acquisition is a
conditional update.

This invariant is **weaker than it looks** and the distinction matters. Two
workers can believe they own the workflow at the same time — that is F5.2 and it
is unavoidable with unsynchronised clocks. What I6 actually guarantees is that
only one of them can *write*. Stating it as "only one worker runs at a time"
would be false, and building on that false belief is how systems with leases but
no fencing corrupt themselves.

---

## Activities

### I7 — A successful result is never overwritten

> Once an activity attempt reaches `SUCCEEDED`, its recorded output does not
> change.

**Enforced by:** attempt rows are immutable once terminal; retries insert new
rows.
**Checked by:** an update attempt against a succeeded row expects an error.

### I8 — Attempt numbers are dense and increasing

> Attempts for one activity are numbered 1..k with no gaps.

**Checked by:** a property test across crash and retry sequences.

---

## Effects

### I9 — A confirmed effect is never reissued under a different key

> If an effect is `CONFIRMED` for key k, no later attempt at the same logical
> activity issues a request with a key other than k.

**Enforced by:** the key is derived deterministically from the workflow ID and
the activity's call position, never from the attempt number or a clock. See
ADR-003.
**Checked by:** a test that crashes across every attempt boundary and asserts
the set of keys sent to the tool simulator has size 1.

This is the single most important invariant in the project. If it breaks,
duplicate side effects follow immediately.

### I10 — `PREPARED` precedes the request

> No external request is sent before a `PREPARED` row is committed for it.

**Enforced by:** the tool adapter takes the key from a ledger handle that can
only be obtained after the commit.
**Checked by:** the tool simulator records a timestamp per request; the test
asserts every request's timestamp follows its ledger row's `created_at`.

### I11 — Effect states never regress

> The transitions forbidden in `state-machines.md` §3 never occur.

**Checked by:** a property test over random transition sequences, plus a
database trigger that rejects the forbidden ones outright.

### I12 — Completion requires certainty

> A workflow reaches `COMPLETED` only when every required effect is in a
> terminal, *known* state.

`UNKNOWN` is not terminal for this purpose. A workflow with an unresolved effect
goes to `MANUAL_RECONCILIATION`, never to `COMPLETED`.
**Checked by:** an invariant sweep after every benchmark run.

---

## Checkpoints

### I13 — A checkpoint names a log position

> Every checkpoint records the event sequence number it corresponds to, and that
> event exists.

**Enforced by:** a foreign key onto `(workflow_id, sequence_no)`.

### I14 — Checkpoint plus tail equals full replay

> Loading checkpoint at sequence s and applying events s+1..n yields the same
> state as replaying 1..n.

**Checked by:** the Phase 7 state-hash equality test. This is what makes
checkpoints safe to trust, and it is the test most likely to catch a subtle bug
in the workflow state representation.

---

## Global

### I15 — A terminal workflow stays terminal

> No transition leaves `COMPLETED` or `FAILED`.

### I16 — Time is not load-bearing

> No correctness decision depends on comparing timestamps from two different
> machines.

**Checked by:** a chaos run with deliberately skewed container clocks. The run
must produce the same correctness outcomes as an unskewed run, though recovery
latency may differ.

---

## Invariant checker

From Phase 3 onward, every benchmark run ends with a sweep that checks I2, I3,
I5, I8, I11, I12, I13 and I15 against the final database state. A run with any
violation is **excluded from results and investigated**, not quietly dropped.
The count of excluded runs is itself reported, since silently discarding failed
runs is one of the easiest ways to produce dishonest benchmark numbers.
