package workloads

import (
	"context"
	"errors"
	"testing"

	"github.com/itsmesamir/resilientflow/adapters/model/fake"
	"github.com/itsmesamir/resilientflow/adapters/tools/booking"
	"github.com/itsmesamir/resilientflow/internal/workflow"
)

func shipment() FreightInput {
	return FreightInput{Origin: "SEA", Dest: "PDX", WeightKg: 250}
}

type harness struct {
	model   *fake.Deterministic
	booking *booking.Simulator
	freight *Freight
}

func newHarness(supportsIdempotency bool, class workflow.Class) *harness {
	m := fake.NewDeterministic()
	b := booking.New(supportsIdempotency)
	return &harness{
		model:   m,
		booking: b,
		freight: &Freight{Model: m, Booking: b, BookingClass: class},
	}
}

func (h *harness) run(runID string, j workflow.Journal) workflow.Result[FreightOutput] {
	res, _ := workflow.Run(context.Background(), h.freight.Definition(), runID, shipment(), j, 1)
	return res
}

// runCrashing executes the workflow and kills it at the named activity,
// simulating a worker that dies mid-flight.
func runCrashing(h *harness, runID string, j workflow.Journal, crashAt string) (crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			crashed = true
		}
	}()
	wctx := workflow.NewContext(context.Background(), runID, j, 1)
	wctx.BeforeActivity = func(name string, _ int) {
		if name == crashAt {
			panic("simulated worker crash at " + name)
		}
	}
	workflow.RunWith(h.freight.Definition(), shipment(), wctx)
	return false
}

// ---------------------------------------------------------------------------
// Happy path
// ---------------------------------------------------------------------------

func TestFreightCompletes(t *testing.T) {
	h := newHarness(true, workflow.Idempotent)
	j := workflow.NewMemoryJournal()

	res := h.run("run-1", j)

	if res.Err != nil {
		t.Fatalf("workflow failed: %v", res.Err)
	}
	if res.State != workflow.Completed {
		t.Fatalf("state is %s, want COMPLETED", res.State)
	}
	if res.Output.BookingID == "" {
		t.Fatal("no booking ID returned")
	}
	if h.booking.Count() != 1 {
		t.Fatalf("created %d bookings, want 1", h.booking.Count())
	}
	if h.model.Calls() != 1 {
		t.Fatalf("made %d model calls, want 1", h.model.Calls())
	}
	if res.Executed != 6 {
		t.Fatalf("executed %d activities, want 6", res.Executed)
	}
	if v := workflow.CheckInvariants(j); len(v) != 0 {
		t.Fatalf("invariant violations: %v", v)
	}
}

// TestReplayCostsNothing: re-running a completed workflow against its own
// journal must execute nothing, call no model, and create no booking.
func TestReplayCostsNothing(t *testing.T) {
	h := newHarness(true, workflow.Idempotent)
	j := workflow.NewMemoryJournal()

	first := h.run("run-1", j)
	if first.Err != nil {
		t.Fatalf("first run: %v", first.Err)
	}
	h.model.Reset()

	second := h.run("run-1", j)
	if second.Err != nil {
		t.Fatalf("replay: %v", second.Err)
	}

	if h.model.Calls() != 0 {
		t.Fatalf("replay made %d model calls, want 0", h.model.Calls())
	}
	if h.booking.Count() != 1 {
		t.Fatalf("replay left %d bookings, want 1", h.booking.Count())
	}
	if second.Executed != 0 {
		t.Fatalf("replay executed %d activities, want 0", second.Executed)
	}
	if second.Replayed != 6 {
		t.Fatalf("replay served %d activities from the journal, want 6", second.Replayed)
	}
	if second.Output != first.Output {
		t.Fatalf("replay produced %+v, original produced %+v", second.Output, first.Output)
	}
}

// ---------------------------------------------------------------------------
// The lost response
// ---------------------------------------------------------------------------

// TestLostResponseBlocksCompletion encodes invariant I12.
//
// The booking was created; the response was lost. The runtime does not know
// whether the effect happened, so it must NOT report success. It goes to
// MANUAL_RECONCILIATION instead.
func TestLostResponseBlocksCompletion(t *testing.T) {
	h := newHarness(true, workflow.Idempotent)
	j := workflow.NewMemoryJournal()
	h.booking.DropAllResponses(true)

	res := h.run("run-1", j)

	if res.State == workflow.Completed {
		t.Fatal("a workflow with an unresolved effect must never report COMPLETED")
	}

	// And specifically not FAILED. The workflow function did return an error —
	// the call failed from its point of view — but reporting FAILED would
	// assert that the booking does not exist, which we have no basis for.
	// MANUAL_RECONCILIATION is the only honest answer.
	if res.State == workflow.Failed {
		t.Fatal("an unresolved effect must not be demoted to FAILED: that is the duplicate-booking bug")
	}
	if res.State != workflow.ManualReconciliation {
		t.Fatalf("state is %s, want MANUAL_RECONCILIATION", res.State)
	}
	if !errors.Is(res.Err, workflow.ErrEffectUnresolved) {
		t.Fatalf("got %v, want ErrEffectUnresolved", res.Err)
	}
	if h.booking.Count() != 1 {
		t.Fatalf("the external world holds %d bookings — the effect DID happen", h.booking.Count())
	}

	effects := workflow.Effects(j)
	var sawUnknown bool
	for _, st := range effects {
		if st == workflow.EffectUnknown {
			sawUnknown = true
		}
	}
	if !sawUnknown {
		t.Fatal("the lost response must be recorded as UNKNOWN, never as FAILED")
	}
}

// ---------------------------------------------------------------------------
// Baseline A versus durable resume — the central comparison
// ---------------------------------------------------------------------------

// TestBaselineRestartFromZeroDuplicates is the failure this project exists to
// prevent, reproduced end to end.
//
// The worker commits the booking, loses the response, and dies. With no durable
// history the restart is a NEW run: new run ID, therefore new idempotency keys,
// therefore the booking service sees an unrelated request. Two bookings.
//
// This is the Twilio 2013 incident in miniature, and the Terraform duplicate
// EC2 bug, and every double charge in docs/related-work.md §1.
func TestBaselineRestartFromZeroDuplicates(t *testing.T) {
	h := newHarness(true, workflow.Idempotent)
	h.booking.DropAllResponses(true)

	// First attempt. The booking commits; the response is lost.
	first := h.run("run-1", workflow.NewMemoryJournal())
	if first.State == workflow.Completed {
		t.Fatal("precondition: the first attempt should not have completed")
	}
	if h.booking.Count() != 1 {
		t.Fatalf("precondition: want 1 booking after the first attempt, got %d", h.booking.Count())
	}

	// The process died. Memory went with it. Restarting from zero means a new
	// run identity, because nothing survived that could tell us otherwise.
	h.booking.DropAllResponses(false)
	second := h.run("run-2", workflow.NewMemoryJournal())
	if second.Err != nil {
		t.Fatalf("restart failed: %v", second.Err)
	}

	if h.booking.Count() != 2 {
		t.Fatalf("got %d bookings, want 2 — restart-from-zero MUST duplicate", h.booking.Count())
	}
	if h.booking.DistinctKeys() != 2 {
		t.Fatal("the duplication is visible as two distinct keys for one logical booking")
	}
	if h.model.Calls() != 2 {
		t.Fatalf("made %d model calls, want 2 — the expensive step was paid for twice", h.model.Calls())
	}
}

// TestDurableResumeDoesNotDuplicate is the same fault with the journal
// preserved, which is what Phase 2 makes real.
//
// Same run ID, same positions, therefore the same idempotency key. The booking
// service recognises the retry and returns the original booking.
func TestDurableResumeDoesNotDuplicate(t *testing.T) {
	h := newHarness(true, workflow.Idempotent)
	j := workflow.NewMemoryJournal()
	h.booking.DropAllResponses(true)

	first := h.run("run-1", j)
	if first.State == workflow.Completed {
		t.Fatal("precondition: the first attempt should not have completed")
	}
	if h.booking.Count() != 1 {
		t.Fatalf("precondition: want 1 booking, got %d", h.booking.Count())
	}
	h.model.Reset()

	// Recovery: same run, same journal, same keys.
	h.booking.DropAllResponses(false)
	second := h.run("run-1", j)

	if second.Err != nil {
		t.Fatalf("resume failed: %v", second.Err)
	}
	if second.State != workflow.Completed {
		t.Fatalf("state is %s, want COMPLETED", second.State)
	}
	if h.booking.Count() != 1 {
		t.Fatalf("got %d bookings, want 1 — the whole point", h.booking.Count())
	}
	if h.booking.DistinctKeys() != 1 {
		t.Fatalf("saw %d distinct keys, want 1 (invariant I9)", h.booking.DistinctKeys())
	}
	if h.model.Calls() != 0 {
		t.Fatalf("resume made %d model calls, want 0 — the recorded answer was reused", h.model.Calls())
	}
	if v := workflow.CheckInvariants(j); len(v) != 0 {
		t.Fatalf("invariant violations: %v", v)
	}
}

// TestNonIdempotentProviderDuplicatesOnRetry is the honest limit of the design.
//
// Stable keys do not help when the provider ignores them. The runtime cannot
// make an uncooperative external system safe; it can only know that it cannot,
// which is why NON_IDEMPOTENT exists as a class.
func TestNonIdempotentProviderDuplicatesOnRetry(t *testing.T) {
	h := newHarness(false, workflow.NonIdempotent)
	j := workflow.NewMemoryJournal()
	h.booking.DropAllResponses(true)

	_ = h.run("run-1", j)
	if h.booking.Count() != 1 {
		t.Fatalf("precondition: want 1 booking, got %d", h.booking.Count())
	}

	h.booking.DropAllResponses(false)
	_ = h.run("run-1", j)

	if h.booking.Count() != 2 {
		t.Fatalf("got %d bookings, want 2", h.booking.Count())
	}
	if h.booking.DistinctKeys() != 1 {
		t.Fatal("the key was stable; the provider simply ignored it")
	}
	// The lesson: Phase 6 must reconcile rather than retry for this class.
}

// ---------------------------------------------------------------------------
// Crash mid-workflow
// ---------------------------------------------------------------------------

// TestCrashLosesEverythingWithoutDurability is Baseline A stated plainly, and
// the reason every later phase exists.
func TestCrashLosesEverythingWithoutDurability(t *testing.T) {
	h := newHarness(true, workflow.Idempotent)

	// The journal dies with the process, so the restart gets a fresh one.
	if !runCrashing(h, "run-1", workflow.NewMemoryJournal(), "create_booking") {
		t.Fatal("expected the worker to crash")
	}
	if h.model.Calls() != 1 {
		t.Fatalf("made %d model calls before the crash, want 1", h.model.Calls())
	}

	res := h.run("run-2", workflow.NewMemoryJournal())
	if res.Err != nil {
		t.Fatalf("restart failed: %v", res.Err)
	}

	if res.Executed != 6 {
		t.Fatalf("restart executed %d activities, want 6 — all progress was lost", res.Executed)
	}
	if h.model.Calls() != 2 {
		t.Fatalf("made %d model calls in total, want 2 — the expensive step was repeated", h.model.Calls())
	}
}

// TestCrashBeforeExternalCallIsSafe: crashing before the side effect leaves the
// outside world untouched, so recovery is unambiguous. Contrast with a crash
// after the call, which is the hard case.
func TestCrashBeforeExternalCallIsSafe(t *testing.T) {
	h := newHarness(true, workflow.Idempotent)
	j := workflow.NewMemoryJournal()

	if !runCrashing(h, "run-1", j, "create_booking") {
		t.Fatal("expected a crash")
	}
	if h.booking.Count() != 0 {
		t.Fatalf("the crash happened before the call; %d bookings exist", h.booking.Count())
	}

	h.model.Reset()
	res := h.run("run-1", j)

	if res.Err != nil {
		t.Fatalf("resume failed: %v", res.Err)
	}
	if h.booking.Count() != 1 {
		t.Fatalf("got %d bookings, want 1", h.booking.Count())
	}
	if h.model.Calls() != 0 {
		t.Fatalf("resume made %d model calls, want 0", h.model.Calls())
	}
	if res.Replayed < 4 {
		t.Fatalf("resume replayed only %d activities; the prefix should have been reused", res.Replayed)
	}
}

// TestInputMismatchIsDetected: if a replay reaches an activity with different
// input, the divergence is surfaced rather than silently accepted. The key is
// unchanged, so no duplicate can result — but the workflow is told.
func TestInputMismatchIsDetected(t *testing.T) {
	h := newHarness(true, workflow.Idempotent)
	j := workflow.NewMemoryJournal()

	if res := h.run("run-1", j); res.Err != nil {
		t.Fatalf("first run: %v", res.Err)
	}

	// A different shipment replayed against the first run's history.
	res, _ := workflow.Run(context.Background(), h.freight.Definition(), "run-1",
		FreightInput{Origin: "SFO", Dest: "LAX", WeightKg: 999}, j, 1)

	if !errors.Is(res.Err, workflow.ErrInputMismatch) {
		t.Fatalf("got %v, want ErrInputMismatch", res.Err)
	}
	if h.booking.Count() != 1 {
		t.Fatalf("divergence created %d bookings, want 1", h.booking.Count())
	}
}
