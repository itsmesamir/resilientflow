package booking

import (
	"context"
	"errors"
	"testing"
)

func req() Request {
	return Request{CarrierID: "C-001", Origin: "SEA", Dest: "PDX", PriceUSD: 400}
}

func TestCreateMakesOneBooking(t *testing.T) {
	s := New(true)
	b, err := s.Create(context.Background(), "key-1", req())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if b.ID == "" {
		t.Fatal("booking has no ID")
	}
	if s.Count() != 1 {
		t.Fatalf("created %d bookings, want 1", s.Count())
	}
}

// TestIdempotentRetryCollapses: a provider that honours keys turns a retry into
// a no-op that returns the original booking.
func TestIdempotentRetryCollapses(t *testing.T) {
	s := New(true)
	ctx := context.Background()

	first, err := s.Create(ctx, "key-1", req())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.Create(ctx, "key-1", req())
	if err != nil {
		t.Fatalf("retry: %v", err)
	}

	if first.ID != second.ID {
		t.Fatalf("retry returned a different booking: %s then %s", first.ID, second.ID)
	}
	if s.Count() != 1 {
		t.Fatalf("created %d bookings, want 1", s.Count())
	}
}

// TestWithoutIdempotencySupportRetryDuplicates is the control case. Against a
// provider with no key support, the identical retry creates a second booking —
// which is why NON_IDEMPOTENT activities must never be blindly replayed.
func TestWithoutIdempotencySupportRetryDuplicates(t *testing.T) {
	s := New(false)
	ctx := context.Background()

	if _, err := s.Create(ctx, "key-1", req()); err != nil {
		t.Fatalf("first: %v", err)
	}
	if _, err := s.Create(ctx, "key-1", req()); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if s.Count() != 2 {
		t.Fatalf("created %d bookings, want 2 — this control case must duplicate", s.Count())
	}
}

// TestDroppedResponseStillCommits is the failure the whole project exists for
// (F2.3, and the Twilio 2013 incident in miniature).
//
// The caller sees an error. The booking exists anyway. Nothing in the caller's
// own state can distinguish this from a request that never arrived.
func TestDroppedResponseStillCommits(t *testing.T) {
	s := New(true)
	s.DropResponseFor("key-1")

	b, err := s.Create(context.Background(), "key-1", req())

	if err == nil {
		t.Fatal("expected the dropped response to surface as an error")
	}
	if !errors.Is(err, ErrResponseDropped) {
		t.Fatalf("got %v, want ErrResponseDropped", err)
	}
	if b != nil {
		t.Fatal("caller must receive no booking when the response is lost")
	}

	// And yet:
	if s.Count() != 1 {
		t.Fatalf("the external world holds %d bookings, want 1 — the effect DID happen", s.Count())
	}
}

// TestReconciliationAfterDroppedResponse: the escape hatch. A provider offering
// lookup-by-key turns an UNKNOWN effect into a known one without guessing.
func TestReconciliationAfterDroppedResponse(t *testing.T) {
	s := New(true)
	s.DropResponseFor("key-1")

	if _, err := s.Create(context.Background(), "key-1", req()); err == nil {
		t.Fatal("expected a dropped response")
	}

	found, ok := s.FindByKey("key-1")
	if !ok {
		t.Fatal("reconciliation failed to find the booking that was actually created")
	}
	if found.IdempotencyKey != "key-1" {
		t.Fatalf("found booking carries key %q", found.IdempotencyKey)
	}
	if s.Count() != 1 {
		t.Fatal("reconciliation must not create anything")
	}
}

// TestRetryAfterDroppedResponseDoesNotDuplicate is the positive result the
// runtime claims: with a stable key, recovering from a lost response is safe.
func TestRetryAfterDroppedResponseDoesNotDuplicate(t *testing.T) {
	s := New(true)
	ctx := context.Background()
	s.DropResponseFor("key-1")

	if _, err := s.Create(ctx, "key-1", req()); err == nil {
		t.Fatal("expected a dropped response")
	}

	// Recovery retries with the SAME key.
	b, err := s.Create(ctx, "key-1", req())
	if err != nil {
		t.Fatalf("recovery retry: %v", err)
	}
	if b == nil {
		t.Fatal("recovery retry returned no booking")
	}
	if s.Count() != 1 {
		t.Fatalf("created %d bookings, want 1", s.Count())
	}
}

// TestRetryWithFreshKeyDuplicates is the counterfactual that justifies ADR-003.
//
// Same scenario, same provider, same idempotency support — but recovery derives
// a NEW key, as it would if the key included the attempt number or a hash of a
// re-synthesized request. Two bookings.
func TestRetryWithFreshKeyDuplicates(t *testing.T) {
	s := New(true)
	ctx := context.Background()
	s.DropResponseFor("key-attempt-1")

	if _, err := s.Create(ctx, "key-attempt-1", req()); err == nil {
		t.Fatal("expected a dropped response")
	}

	if _, err := s.Create(ctx, "key-attempt-2", req()); err != nil {
		t.Fatalf("retry: %v", err)
	}

	if s.Count() != 2 {
		t.Fatalf("created %d bookings, want 2", s.Count())
	}
	if s.DistinctKeys() != 2 {
		t.Fatal("this failure is visible as two distinct keys for one logical booking")
	}
}

func TestRejectionCreatesNothing(t *testing.T) {
	s := New(true)
	s.RejectFor("key-1")

	_, err := s.Create(context.Background(), "key-1", req())
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("got %v, want ErrRejected", err)
	}
	if s.Count() != 0 {
		t.Fatalf("a definite rejection created %d bookings, want 0", s.Count())
	}
}

// TestRequestLogRecordsLostResponses: the oracle the benchmark depends on. The
// service's own record of what it saw is the ground truth against which the
// runtime's beliefs are checked.
func TestRequestLogRecordsLostResponses(t *testing.T) {
	s := New(true)
	s.DropResponseFor("key-1")

	_, _ = s.Create(context.Background(), "key-1", req())

	log := s.Requests()
	if len(log) != 1 {
		t.Fatalf("logged %d requests, want 1", len(log))
	}
	if !log[0].CreatedBooking {
		t.Error("the log must record that a booking was created")
	}
	if !log[0].ResponseLost {
		t.Error("the log must record that the response was lost")
	}
}

func TestContextCancellationIsRespected(t *testing.T) {
	s := New(true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Create(ctx, "key-1", req()); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if s.Count() != 0 {
		t.Fatal("a cancelled call must not create a booking")
	}
}
