# Design Decisions

One record per decision. Format: context, options, decision, consequences.
A decision marked **PROPOSED** needs confirmation before the phase that depends
on it begins. A decision marked **ACCEPTED** may still be revised, but the
revision gets its own record rather than an edit in place.

---

## ADR-001 — Workflow programming model

**Status:** PROPOSED. Must be settled before Phase 1.
**Blocks:** everything.

### Context

The natural API returns activity results inline:

```go
candidates := ctx.Execute("retrieve_candidates", PURE, retrieveCandidates, input)
ranked     := ctx.Execute("rank_candidates",     PURE, rankCandidates, candidates)
```

Ordinary-looking code, but it hides a fundamental choice about how recovery
works.

### Options

**A. Replay with memoization** (Temporal's model). On recovery the workflow
function is re-executed from the top. Each `ctx.Execute` consults the event log:
if a result was recorded for that call, it is returned immediately without
running anything.

- Workflow code reads naturally.
- Requires strict determinism in workflow code: no clocks, no random numbers, no
  map iteration order, no direct I/O. All of those must go through `ctx`.
- Makes the Temporal comparison in Phase 14 a genuine like-for-like.
- Changing workflow code between the original run and the replay breaks it,
  which is Stretch E.

**B. Explicit state machine.** The workflow is declared as a list or graph of
steps and the coordinator advances it one step at a time.

- State is fully inspectable, and no determinism discipline is needed.
- Recovery is trivial: read the current step and continue.
- Loops, conditionals and branching become awkward to express.
- Diverges from the natural API and makes the Temporal comparison less direct.

**C. Hybrid.** Option B internally, with A's API as sugar over it. Rejected:
this is the most work and inherits the constraints of both.

### Decision

**Option A.** The research question is what a runtime should do *at each
decision point* when it meets an unfinished activity. Option A makes every
`ctx.Execute` exactly such a decision point, which puts selective replay at the
centre of the design instead of bolting it on. Option B would make the
engineering easier and the research weaker.

### Consequences

- A determinism contract must be written before Phase 1 and enforced by a
  linter or a runtime guard.
- `ctx` must provide `Now()`, `Random()` and `UUID()`, each recording its result
  in the log so replay reproduces it.
- Replay cost grows with workflow length, which is precisely why checkpointing
  in Phase 7 is needed and gives it something real to measure.
- A `NONDETERMINISTIC_EXPENSIVE` activity is handled by the same memoization
  path as a `PURE` one. Their difference shows up in *cost*, not mechanism,
  which is worth being explicit about in the report.

---

## ADR-002 — The event log is the source of truth

**Status:** ACCEPTED.

### Context

The guide's schema has `workflow_events` alongside `activity_executions` and
`effect_ledger`. Nothing in the guide says which wins when they disagree, and
they will disagree if they are written separately.

### Decision

The event log is authoritative. The other tables are **projections**, written in
the same transaction as the event that causes them. They exist for query
efficiency, not as independent state.

### Consequences

- Every state change is an event append plus a projection update, in one
  transaction. Never one without the other.
- Invariant I3 becomes checkable: rebuild the projections and compare.
- Write amplification roughly doubles. That cost is measurable and gets reported
  rather than hidden.
- If a projection is ever found to disagree with the log, the log wins and the
  projection is rebuilt.

---

## ADR-003 — Idempotency key derivation

**Status:** ACCEPTED. The most consequential decision in the project.

### Context

The key must be identical across every attempt at the same logical operation,
including attempts made by *different workers* that cannot talk to each other.
A stale worker and the new owner must independently arrive at the same key, or
fencing in the database will not prevent a duplicate at the tool.

### Options

| Candidate | Fails because |
|---|---|
| Random UUID per attempt | Every retry is a new operation. Guarantees duplicates |
| Includes attempt number | Same problem, made to look deliberate |
| Includes a timestamp | Two workers compute different keys |
| `hash(workflow_id, call_seq)` | Works |
| `hash(workflow_id, call_seq, input_hash)` | Works, but see below |

### Decision

```
key = hash(workflow_id, activity_call_seq)
```

where `activity_call_seq` is the position of the `ctx.Execute` call in the
deterministic execution order, assigned by the runtime, not by the author.

The input hash is **recorded separately** on the ledger row, and compared on
retry, but is deliberately not part of the key.

### Why input is excluded from the key

Tempting, since a different input is arguably a different operation. But it
creates a failure mode: if any upstream step produces a subtly different value
on replay — a re-serialised float, a reordered map — the key changes and we send
a second request under a new key. The very nondeterminism the runtime exists to
tolerate would defeat the mechanism meant to protect against it.

Recording the input hash separately gives the best of both: a mismatch is
*detected and reported* as failure F6.3, but it does not silently mint a new
key.

### External validation

This reasoning was arrived at independently; published incidents and research
support it from opposite directions.

- **Production.** PostHog's April 2026 postmortem: their dedup key was rewritten
  by a queue boundary (UUIDT → UUIDv7), legitimate resumes were read as
  duplicates, and **11,920 invocations were silently dropped** across 33
  customer organisations. The failure was silent, which is worse than the
  duplicates the mechanism existed to prevent.
- **Research.** ACRFence (arXiv:2603.20625) reports that "make tool calls safe
  to retry" fails for agents, because after a restore the agent re-synthesizes a
  subtly different request that the server treats as new — and that none of 12
  surveyed agent frameworks enforce exactly-once at the tool boundary.
- **Practice.** Stripe's 2025 post names generating a key "per network attempt
  rather than per business operation" as a leading misuse.

All three say the same thing: **a key derived from anything that can change
between the decision and the effect does not work.** Position-derived keys are
the structural answer, and no evaluation of them under distributed fault
injection was found.

The separately recorded input hash, written here as a footnote, may turn out to
be the more interesting half — it is the only instrument that can detect the
divergence this design deliberately accepts, and measuring how often it occurs
is an open question.

### Consequences

- `activity_call_seq` must be deterministic, which is a hard dependency on
  ADR-001's determinism contract.
- Loops need care: the nth iteration's call must get a distinct, reproducible
  sequence number.
- Invariant I9 is the test for this decision, and it is the test that most
  directly measures whether the project's central claim holds.

---

## ADR-004 — Two kinds of lease

**Status:** ACCEPTED.

### Context

The guide has a single `workflow_leases` table naming a worker, while the
architecture gives the coordinator the state machine. It is unclear what owns
what.

### Decision

Two distinct concepts:

1. **Workflow ownership lease** — who may advance this workflow's state machine
   and write to its log. Carries the fencing epoch. Held by whichever process
   runs the workflow function (under ADR-001, that is a worker).
2. **Activity task claim** — who is executing this one task. Short-lived, from
   the queue via `FOR UPDATE SKIP LOCKED`, and carries no epoch.

Only the first fences. A task claim that outlives its workflow lease is
harmless, because any write it attempts is rejected by the epoch check.

### Consequences

Clarifies what happens when a worker finishes a task after losing ownership: the
work was done, the result is discarded, the effect ledger still holds the
record, and recovery will find it.

---

## ADR-005 — Baseline set

**Status:** ACCEPTED.

### Context

The obvious baselines are restart, retry, and fixed-interval checkpointing.
Against a tool that supports idempotency keys, beating plain retry is trivially
true and proves nothing about this runtime's actual contribution.

### Decision

Four baselines, adding one the guide omits:

| Baseline | Isolates |
|---|---|
| A. Restart from zero | The value of durability at all |
| B. Retry failed activity | The value of persisting results |
| **B+. Retry with idempotency keys** | **What semantic classification adds over keys alone** |
| C. Fixed-interval checkpoint | The value of adaptive placement |
| ResilientFlow | The combination |

### Consequences

B+ is the honest comparison and may well show a small margin. If the margin is
small, that is the result and it gets reported. The interesting cases will be
tools that do *not* support keys, where B+ has no answer and must either
duplicate or stall.

---

## ADR-006 — Activity class is declared by the adapter

**Status:** ACCEPTED.

### Context

Whether `book` is idempotent depends on whether the provider accepts a stable
key. So the class is a property of the tool, not of the call site — yet the
natural API has the author pass it at each call.

### Decision

The tool adapter declares its own replay class. The call site may *narrow* it
(declaring a stricter class than the adapter claims) but may never widen it.
`READ_ONLY` is a sixth class: safe to replay, no external effect, but not
necessarily the same answer twice.

### Consequences

- A mislabelling test becomes possible and should be run: declare a
  non-idempotent tool as idempotent and measure what the runtime does. A runtime
  whose safety collapses entirely on one wrong label is worth knowing about, and
  is an honest limitation for the report.
- Class metadata belongs with the adapter and is recorded in the event log at
  execution time, so a later change to the adapter does not rewrite history.

---

## ADR-007 — Recovery acquires the epoch first

**Status:** ACCEPTED.

### Context

The obvious ordering acquires the fencing epoch late — after loading the
checkpoint, classifying activities, and inspecting the ledger.

### Decision

The epoch is acquired **first**, before anything else is read.

### Why

With the epoch last, two workers that both detect an expired lease would each
run the whole procedure. Both would inspect the ledger, both would find the same
`UNKNOWN` effect, and both could issue a compensating action. Compensations are
rarely idempotent, so this can be worse than the original fault.

### Consequences

Recovery reads consistent state, since anything it reads is protected by
ownership it already holds.

---

## ADR-008 — Reproducibility baseline

**Status:** ACCEPTED.

- Postgres pinned to `17.7-alpine`; Go pinned via `go.mod`.
- Benchmarks run on Docker Compose, not Kubernetes. Less scheduling noise and
  simpler metadata capture. Kubernetes is for the failure demo only.
- Every result row carries: git commit, config hash, random seed, Go version,
  Postgres version, worker count, workflow length, lease TTL, failure policy,
  checkpoint policy, host CPU and memory.
- Raw results are committed as CSV. Plots are regenerated from them and are
  never the primary artifact.

---

## Open, not yet decided

| Question | Needed by |
|---|---|
| Lease TTL default, and whether it adapts | Phase 4 |
| Retry budget scope: per activity, per workflow, or both | Phase 3 |
| State representation for checkpoints: full snapshot or delta | Phase 7 |
| Whether a reconciliation sweep can itself be crash-safe and idempotent | Phase 6 |
| Policy when a replayed model output differs from the record (F6.3) | Phase 9 |
