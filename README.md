# ResilientFlow

A fault-tolerant runtime for long-running stateful workflows, built to study one
question: **what should a workflow engine do when it crashes around an external
side effect it cannot verify?**

Go · PostgreSQL · property-based testing · deterministic fault injection

---

## The problem

A workflow books freight. At step five it sends `POST /booking`. The service
creates reservation `R123`. Before the response arrives, the worker dies.

A new worker picks up the workflow. It knows a booking was attempted. It cannot
tell which of these happened:

```
(a) the request never arrived                  → no booking exists
(b) it arrived and was rejected                → no booking exists
(c) it arrived, committed, the reply was lost  → booking R123 exists
(d) it is still being processed                → booking may yet exist
```

Retry and you may book twice. Don't retry and the workflow may never finish. No
amount of local state resolves this — the fact you need is held by the other
party, and the channel to that party is what failed.

This is not a hypothetical. Twilio's 2013 billing incident charged 1.4% of all
customers repeatedly because card charges committed while the balance write kept
failing. Segment reported ~0.6% of ingested events as duplicates, costing 1.5 TB
of deduplication keys to suppress. Every durable execution vendor draws its
guarantee at the journal write and hands the rest back to the developer —
Temporal's own documentation states that "lack of idempotency doesn't lead to a
platform error."

## What it does

```console
$ make demo

── Baseline: restart from zero
   attempt 1 → state=MANUAL_RECONCILIATION
              bookings in the external system: 1
              the effect happened; the worker does not know it

   attempt 2 → executed=6 replayed=0, model calls: 1
              distinct idempotency keys seen by the service: 2
              bookings: 2  ✗ 2 BOOKINGS — the customer was charged twice

── Durable resume: same run, same keys
   attempt 2 → executed=2 replayed=4, model calls: 0
              distinct idempotency keys seen by the service: 1
              bookings: 1  ✓ one booking
```

Nothing differs between those two runs — same service, same network, same fault.
The only difference is whether the retry could derive the **same idempotency key**
as the original attempt.

## How it works

| Mechanism | Answers |
|---|---|
| Append-only event journal | the worker died and forgot everything |
| Leases with monotonic fencing epochs | a stale worker corrupting its successor's state |
| Transactional outbox | state committed but work never enqueued |
| Effect ledger with an explicit `UNKNOWN` state | the effect happened but we have no record of it |
| Semantic activity classes | not all steps are equally safe to redo |
| Selective replay | resuming without repeating what must not be repeated |

Each activity declares how it may be replayed — `PURE`, `READ_ONLY`,
`IDEMPOTENT`, `COMPENSATABLE`, `NON_IDEMPOTENT`, `NONDETERMINISTIC_EXPENSIVE` —
and recovery treats each class differently. The class is declared by the tool
adapter, not the call site, because whether booking is idempotent is a property
of the provider.

## The decision it turns on

Idempotency keys are derived as:

```go
key = hash(runID, activityCallSeq)
```

The run's identity and the step's **position in the execution**. Deliberately
not the attempt number, not a timestamp, and **not a hash of the input**.

Excluding the input is the non-obvious part. If an upstream step produces a
subtly different value on replay — a re-serialised float, a regenerated
request — a content-derived key changes with it, and a second request goes out
under a new key. The mechanism meant to prevent duplication is defeated by the
very nondeterminism it exists to tolerate.

Published evidence points the same way from three directions. PostHog's 2026
postmortem: a queue boundary rewrote their deduplication key, silently dropping
11,920 legitimate invocations. ACRFence (arXiv:2603.20625): "make tool calls safe
to retry" fails for LLM agents because a restored agent re-synthesizes a slightly
different request the server sees as new — and none of 12 surveyed frameworks
enforce exactly-once at the tool boundary. Stripe names generating keys "per
network attempt rather than per business operation" as a leading misuse.

The trade-off this accepts is **intent divergence**: a genuinely different
request arrives under the same key, so the service returns the original effect.
No duplicate, but the effect may no longer match current intent. The input hash
is recorded separately so divergence is detected rather than silent.

See [ADR-003](docs/design-decisions.md).

## Status

**Phase 1 of 8 complete.** The runtime executes workflows with replay and
memoization; the journal is in-memory, so durability across a real process crash
is Phase 2. 48 tests, race-clean.

| Phase | | |
|---|---|---|
| 1 | Workflow runtime, replay classes, memoized replay | ✅ |
| 2 | PostgreSQL event log, state reconstruction | next |
| 3 | Coordinator/worker split, database-backed queue | |
| 4 | Leases and fencing tokens | |
| 5 | Transactional outbox, inbox deduplication | |
| 6 | Effect ledger, reconciliation, compensation | |
| 7 | Checkpointing | |
| 8 | Selective replay and the baseline comparison | |

## Running it

```bash
make demo        # the lost-response scenario, side by side
make test        # unit tests, no database required
make test-all    # with race detector and coverage
```

Go 1.23+. From Phase 2, Postgres via `make up` (requires Docker).

## Design documents

| | |
|---|---|
| [failure-model.md](docs/failure-model.md) | What can fail, how it is detected, what is recoverable, and what requires a human |
| [state-machines.md](docs/state-machines.md) | Workflow, activity and effect transitions — including the ones that must be impossible |
| [invariants.md](docs/invariants.md) | 16 invariants, each with its enforcement mechanism and its test |
| [design-decisions.md](docs/design-decisions.md) | Architecture decision records |

Two bugs during Phase 1 were caught by the project's own checks rather than by
tests written to find them. The invariant checker rejected an `UNKNOWN →
PREPARED` transition when a resumed activity re-wrote its ledger row; and the
runtime initially reported an unresolved effect as `FAILED`, which asserts the
effect did not happen when there was no basis for that. Both are the thesis of
the project appearing as defects in its own implementation.

## What this does not claim

- **Not exactly-once.** That is impossible without cooperation from the external
  system. This provides effectively-once execution where a tool supports stable
  idempotency keys, and explicit uncertainty semantics otherwise.
- **It cannot make an uncooperative service safe.** If a provider offers no key,
  no lookup API and no compensation, no runtime saves you. The contribution is
  knowing and reporting that, rather than guessing.
- **Durable execution is not novel.** Temporal, Azure Durable Task, Restate and
  DBOS all provide it. The narrower question here is what semantic classification
  of effects adds beyond stable idempotency keys alone — which requires measuring,
  not asserting.
- **Not production-ready.** Single Postgres instance, no HA, no security model,
  no multi-tenancy.

## License

MIT
