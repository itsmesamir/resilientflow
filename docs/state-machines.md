# State Machines

Three state machines govern the runtime. They are specified here before any code
exists, because the transition tables become the unit tests in Phase 1 and the
property tests in Phase 8.

Rule for all three: **any transition not listed is illegal.** An attempted
illegal transition is failure F6.4, is logged, and fails the operation loudly.
It is never silently ignored.

---

## 1. Workflow execution

```
                    ┌──────────────────────────────┐
                    ▼                              │
  PENDING ──► RUNNING ──► WAITING ─────────────────┘
                 │ │
                 │ └────────► COMPLETED
                 │
                 ├──────────► RECOVERING ──► RUNNING
                 │
                 ├──────────► FAILED
                 │
                 └──────────► MANUAL_RECONCILIATION ──► RUNNING
                                                   └──► FAILED
```

| From | To | Trigger | Guard |
|---|---|---|---|
| — | `PENDING` | Workflow submitted | — |
| `PENDING` | `RUNNING` | A worker acquires the lease | Epoch assigned |
| `RUNNING` | `WAITING` | `ctx.Sleep` or `WaitForSignal` | No activity in flight |
| `WAITING` | `RUNNING` | Timer fires or signal arrives | New epoch acquired |
| `RUNNING` | `RECOVERING` | Lease expired, another worker takes over | Epoch incremented |
| `RECOVERING` | `RUNNING` | Selective replay finished | No effect left `UNKNOWN` that blocks progress |
| `RECOVERING` | `MANUAL_RECONCILIATION` | An unresolvable `UNKNOWN` effect | Failure F7 |
| `MANUAL_RECONCILIATION` | `RUNNING` | Human resolves the effect | Effect reached a terminal state |
| `MANUAL_RECONCILIATION` | `FAILED` | Human abandons the workflow | Compensations done |
| `RUNNING` | `COMPLETED` | Final activity committed | **All required effects terminal and known** |
| `RUNNING` | `FAILED` | Retry budget exhausted, or a fatal semantic error | Compensations attempted |

### Terminal states

`COMPLETED` and `FAILED` are terminal. There is no transition out of either. A
late message referring to a terminal workflow is discarded, which is failure
F3.2.

### Note on `RECOVERING`

`RECOVERING` is entered by the *new* owner, after it has taken the epoch. The
old owner never sets this state — it may be dead, and if it is merely paused it
must not be able to write at all.

The ordering matters: taking the epoch last would let two workers that both
detect an expired lease each run the recovery procedure, and both issue a
compensating action. Compensations are rarely idempotent, so that can be worse
than the original fault.

---

## 2. Activity attempt

One row per attempt. An attempt is never mutated after it reaches a terminal
state; a retry creates a **new row** with `attempt = n + 1`.

```
  SCHEDULED ──► STARTED ──► SUCCEEDED
                   │
                   ├──────► FAILED_RETRYABLE ──► (new attempt row)
                   │
                   ├──────► FAILED_TERMINAL
                   │
                   └──────► ABANDONED
```

| From | To | Trigger |
|---|---|---|
| — | `SCHEDULED` | Coordinator enqueues the task |
| `SCHEDULED` | `STARTED` | A worker claims it and records its epoch |
| `STARTED` | `SUCCEEDED` | Result committed to the event log |
| `STARTED` | `FAILED_RETRYABLE` | F2.1, F4.x within the retry budget |
| `STARTED` | `FAILED_TERMINAL` | Retry budget exhausted, or a non-retryable error |
| `STARTED` | `ABANDONED` | The owning worker's lease expired |
| `SCHEDULED` | `ABANDONED` | Workflow reached a terminal state first |

### Why `ABANDONED` is separate from `FAILED`

An abandoned attempt tells us nothing about whether its side effect happened.
The worker vanished mid-flight. Collapsing it into `FAILED` would throw away
exactly the information the effect ledger needs, and would reintroduce the bug
described in §3 of the failure model.

An attempt in `ABANDONED` whose activity class is not `PURE` **must** leave a
corresponding row in the effect ledger for recovery to inspect.

---

## 3. Effect ledger

The most important of the three. One row per external side effect.

```
                     ┌──► CONFIRMED ──► (terminal)
                     │
  PREPARED ──────────┼──► UNKNOWN ──┬──► CONFIRMED
                     │              ├──► FAILED
                     │              └──► COMPENSATED
                     │
                     └──► FAILED ──► (terminal)

  CONFIRMED ──► COMPENSATED   (only via an explicit compensating action)
```

| From | To | Trigger |
|---|---|---|
| — | `PREPARED` | Idempotency key committed, **before** the request is sent |
| `PREPARED` | `CONFIRMED` | A definite success response arrived |
| `PREPARED` | `FAILED` | A definite failure response arrived (4xx, explicit rejection) |
| `PREPARED` | `UNKNOWN` | Timeout, lost connection, or the worker died mid-call |
| `UNKNOWN` | `CONFIRMED` | Reconciliation found the effect, or an idempotent retry succeeded |
| `UNKNOWN` | `FAILED` | Reconciliation proved the effect never happened |
| `UNKNOWN` | `COMPENSATED` | A compensating action ran defensively |
| `CONFIRMED` | `COMPENSATED` | Saga rollback |

### The transitions that must be impossible

These are the ones worth writing property tests for:

- `CONFIRMED → PREPARED` — would mean an effect un-happened.
- `CONFIRMED → UNKNOWN` — once known, never un-known.
- `CONFIRMED → FAILED` — the dangerous one. This is the bug that creates
  duplicate bookings.
- `FAILED → CONFIRMED` — only legal via `UNKNOWN`. If a definite rejection later
  turns out to have succeeded, the tool lied, and that is outside our model.
- Any transition into `PREPARED`. It is only ever an initial state.

### Why `PREPARED` is written before the request is sent

If the key were written after the response arrived, a crash between sending and
committing would leave no record that a request was ever made. Recovery would
then have no key to retry with, and would be forced to generate a new one —
producing the duplicate. Writing first costs one extra database round trip per
effect and is what makes F1.2 and F1.3 recoverable at all.

The overhead of that extra write is measurable, and Phase 7 should report it.

---

## 4. Relationship between the three

```
workflow: RUNNING
   └── activity attempt 1: ABANDONED   (worker crashed)
   │      └── effect: UNKNOWN
   ├── recovery: reconcile the effect → CONFIRMED
   └── activity attempt 2: not created — the effect was already confirmed
```

The point of selective replay is the last line. A naive runtime creates attempt
2 and issues the call again. ResilientFlow inspects the ledger first and skips
it. Measuring how often that skip happens, and what it saves, is the experiment.
