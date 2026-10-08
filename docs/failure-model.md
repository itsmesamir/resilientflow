# Failure Model

Status: Phase 0 draft. Every later phase must either satisfy this document or
amend it. If an experiment produces a failure this document does not describe,
the document is wrong and gets updated before the code does.

---

## 1. System model

ResilientFlow is a set of processes that communicate over an asynchronous
network and share one durable store.

```
clients ──► coordinator ──► Postgres ◄── workers ──► external tools
```

| Component | Count | Stateful? |
|---|---|---|
| Coordinator | 1..n | No. All state is in Postgres |
| Worker | 1..n | No. Holds only in-flight activity state |
| Postgres | 1 | Yes. The single source of truth |
| External tool / model | n | Yes, and outside our control |

### Assumptions we make

1. **Asynchronous network.** Messages can be delayed without bound, dropped,
   reordered, or delivered more than once. There is no upper bound on delivery
   time, so a slow process cannot be distinguished from a dead one. This is why
   we need fencing and not only timeouts.
2. **Crash-stop processes.** A coordinator or worker either follows the protocol
   or halts. A halted process may later restart with no memory of what it was
   doing.
3. **Postgres is correct and durable.** A committed transaction survives. We do
   not attempt to tolerate Postgres losing committed data, and we do not
   replicate it. Postgres is our consensus: it is the one place where two
   processes can agree on what happened.
4. **Postgres may be unavailable.** Correct but temporarily unreachable is a
   failure we handle. Permanently lost is not.
5. **Clocks are unsynchronised and may jump.** Wall-clock time is used for lease
   *expiry hints* only. Correctness rests on the monotonic epoch counter in the
   database, never on a timestamp comparison between two machines.

### What we explicitly do not handle

Naming these matters as much as naming the ones we do. Each is out of scope
because it would change the project from a recovery-semantics study into
something else.

| Excluded | Why |
|---|---|
| Byzantine failures | Components are assumed not to lie. Defending against a malicious worker is a different research problem |
| Disk corruption / bit rot | Delegated to Postgres and the filesystem |
| Postgres permanent data loss | Would require replication and a consensus protocol; see Stretch F |
| Network partition *inside* Postgres | Single instance, so no split-brain in the store itself |
| Malicious or compromised external tools | Tools may fail or be slow; they are not assumed hostile |
| Memory-safety bugs in our own code | Handled by tests, not by the recovery protocol |

---

## 2. Failure taxonomy

Each failure gets an ID used in test names, `faultctl` flags, and the experiment
log, so a result can always be traced back to the exact fault that produced it.

### F1 — Crash-stop

| | |
|---|---|
| **F1.1** | Worker killed mid-activity, before the external call |
| **F1.2** | Worker killed mid-activity, after the external call was sent |
| **F1.3** | Worker killed after the external call returned, before the result was committed |
| **F1.4** | Coordinator killed between appending an event and enqueuing the work |
| **F1.5** | Coordinator killed after enqueuing, before marking the outbox row sent |

**How we detect it:** the worker's lease stops being renewed and expires. No
other process can tell a crash from a pause, and it does not need to — the
response is the same either way.

**What the runtime does:** another worker takes ownership, which increments the
epoch, then runs the selective-replay procedure.

F1.3 is the interesting one and the whole reason the effect ledger exists. The
external world moved forward and our records did not.

### F2 — Network

| | |
|---|---|
| **F2.1** | Connection to a tool times out before the request is sent |
| **F2.2** | Request sent, no response within the deadline |
| **F2.3** | Request sent and succeeded remotely, response lost in transit |
| **F2.4** | Response arrives after the deadline has already fired |
| **F2.5** | Worker cannot reach Postgres |
| **F2.6** | Coordinator cannot reach Postgres |

**How we detect it:** context deadline expiry on the client side.

**What the runtime does:** F2.1 is safe to retry — nothing left the process.
F2.2, F2.3 and F2.4 are **indistinguishable from each other at the caller**, and
that is the central difficulty of this project. All three produce an effect in
state `UNKNOWN`. F2.5 and F2.6 mean the process must stop making progress; it
cannot even safely record that it failed, so it must drop its lease and let it
expire.

> A worker that cannot reach Postgres must assume it has already lost its lease.
> It must not continue issuing external calls on the strength of a lease it can
> no longer prove it holds.

### F3 — Message delivery

| | |
|---|---|
| **F3.1** | The same task is delivered twice |
| **F3.2** | A task is delivered after a long delay, when it is no longer relevant |
| **F3.3** | Redelivery after an acknowledgement was lost |
| **F3.4** | Out-of-order delivery of tasks for one workflow |

**How we detect it:** the inbox table, which records processed message IDs.

**What the runtime does:** duplicates are recognised and the recorded result is
returned without re-executing. Late or irrelevant deliveries are rejected by the
epoch check. We never rely on the transport to suppress duplicates, including
after the move to NATS.

### F4 — Dependency

| | |
|---|---|
| **F4.1** | Postgres unavailable for a bounded period |
| **F4.2** | Queue unavailable (once the queue is external) |
| **F4.3** | Model provider returns 429 |
| **F4.4** | Tool returns 5xx |
| **F4.5** | Tool accepts the connection and never responds |

**What the runtime does:** retry with exponential backoff and jitter, under a
per-workflow retry budget so that a failing dependency cannot cause a retry
storm. F4.5 is bounded by the context deadline and then becomes F2.2.

### F5 — Stale worker

| | |
|---|---|
| **F5.1** | Worker pauses (GC, SIGSTOP, VM suspend), lease expires, worker resumes and tries to write |
| **F5.2** | Two workers each believe they own the same workflow |
| **F5.3** | A stale worker issues an external call after losing ownership |

**How we detect it:** the epoch check on every write. A stale write updates zero
rows.

**What the runtime does:** the write is rejected and the worker halts that
workflow. F5.3 cannot be prevented by the database, and is instead contained by
deriving idempotency keys in a way that a stale worker and the new owner both
arrive at the same key. See ADR-003.

### F6 — Semantic

| | |
|---|---|
| **F6.1** | Tool returns output that does not match the expected schema |
| **F6.2** | Model produces invalid tool arguments |
| **F6.3** | Model output differs from the recorded output during replay |
| **F6.4** | An illegal state transition is attempted |
| **F6.5** | Retrieved data has become stale relative to the external world |

These are not infrastructure failures and are counted separately in the metrics.
F6.4 indicates a bug in our runtime, not an environmental fault, and any
occurrence during a benchmark run invalidates that run.

---

## 3. The uncertain-outcome problem

This section exists because it is the reason the project is interesting.

A worker sends a request. No response arrives. The worker knows exactly one
thing: it sent bytes. It cannot distinguish between:

```
(a) request never reached the service          → no effect
(b) service received it and rejected it        → no effect
(c) service received it, committed, reply lost → effect happened
(d) service is still processing it             → effect may yet happen
```

No amount of local state resolves this. The information required is held by the
other party, and the channel to that party is the thing that failed.

The only honest response is to record the effect as `UNKNOWN` and resolve it
through one of five routes:

| Route | Requires | Result |
|---|---|---|
| Idempotency key | Tool supports stable keys | Retry is safe; the key collapses (c) and a retry into one operation |
| Query by operation ID | Tool has a read API keyed by our ID | We learn the truth directly |
| Compensation | The effect is reversible | Undo it, then retry cleanly |
| Reconciliation sweep | Tool has a list/search API | Find the effect by our correlation ID |
| Human decision | None of the above | Workflow moves to `MANUAL_RECONCILIATION` |

The runtime **must not** convert `UNKNOWN` to `FAILED` because a response was
lost. That conversion is exactly the bug that produces double bookings, and
Baseline B is expected to demonstrate it.

---

## 4. Detection mechanisms

| Mechanism | Detects | Delay before detection |
|---|---|---|
| Lease expiry | F1 (any crash), F5.1 | Up to the lease TTL |
| Heartbeat gap | F1, a slow worker | TTL / 3, the renewal interval |
| Context deadline | F2.1–F2.4, F4.5 | The per-activity deadline |
| Epoch check returning 0 rows | F5.1, F5.2, F3.2 | Immediate, at write time |
| Inbox hit | F3.1, F3.3 | Immediate |
| Schema validation | F6.1, F6.2 | Immediate |
| Invariant checker after a run | F6.4 | End of run |

Lease TTL sets a floor on recovery time, so it is a tuning parameter the
benchmark must vary and report. Too short and healthy workers lose leases during
a GC pause; too long and crash recovery is slow.

---

## 5. Recoverability summary

| Class | Recovered automatically | Notes |
|---|---|---|
| F1 crash-stop | Yes | Provided the last committed event is consistent |
| F2.1 pre-send timeout | Yes | Nothing left the process |
| F2.2–F2.4 uncertain | **Only when the tool cooperates** | Otherwise F7 below |
| F2.5–F2.6 Postgres unreachable | Yes, once reachable again | Progress stops meanwhile |
| F3 duplicates | Yes | Inbox plus epoch |
| F4 dependency | Yes, within the retry budget | Then the workflow fails cleanly |
| F5 stale worker | Yes | Fencing |
| F6.1–F6.3 semantic | Policy dependent | May fail the workflow |
| F6.4 illegal transition | No | A bug; fails loudly |

**F7 — requires a human.** An effect that is `UNKNOWN`, against a tool with no
idempotency key, no query API and no compensating action. The workflow enters
`MANUAL_RECONCILIATION` and waits. Counting how often this happens is itself a
result worth reporting: a runtime that never needs a human is either lucky or
lying.

---

## 6. Open questions for later phases

1. What lease TTL best balances false-positive expiry against recovery latency?
   Measured in Phase 4.
2. Should a retry budget be per activity, per workflow, or both?
3. When the model's recorded output conflicts with a regenerated one (F6.3),
   which wins, and does the answer depend on whether an effect has since been
   committed?
4. Can a reconciliation sweep itself be made idempotent and crash-safe?
