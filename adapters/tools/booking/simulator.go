// Package booking simulates an external booking service.
//
// It is the most important test fixture in the project. Everything the runtime
// claims about side-effect safety is claimed against this simulator, so it must
// model the failure that matters: committing an effect and then losing the
// response.
package booking

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Errors returned by the simulator.
var (
	// ErrResponseDropped means the booking WAS created and the response was
	// lost (failure F2.3). It is deliberately NOT a definite failure: from the
	// caller's side it is indistinguishable from a request that never arrived,
	// which is the entire problem.
	ErrResponseDropped = errors.New("booking: connection lost after commit")

	// ErrUnavailable is a transient dependency failure (F4.4). Also not
	// definite — the request may have been processed before the service became
	// unreachable.
	ErrUnavailable = errors.New("booking: service unavailable")

	// ErrRejected is a conclusive refusal: the service considered the request
	// and created nothing. This is the only error here that lets the runtime
	// record FAILED rather than UNKNOWN.
	ErrRejected error = rejected{}
)

// rejected reports a definite failure.
//
// DefiniteFailure satisfies an unexported interface in the workflow package
// structurally, so this adapter reports the distinction without importing it.
type rejected struct{}

func (rejected) Error() string         { return "booking: rejected" }
func (rejected) DefiniteFailure() bool { return true }

// Request is a booking to create.
type Request struct {
	CarrierID string `json:"carrier_id"`
	Origin    string `json:"origin"`
	Dest      string `json:"dest"`
	PriceUSD  int    `json:"price_usd"`
}

// Booking is a created reservation.
type Booking struct {
	ID             string    `json:"id"`
	IdempotencyKey string    `json:"idempotency_key"`
	Request        Request   `json:"request"`
	CreatedAt      time.Time `json:"created_at"`
}

// Simulator is an in-process booking service.
//
// SupportsIdempotency models the difference between a provider that accepts a
// stable key and one that does not. That single flag decides whether an
// activity is IDEMPOTENT or NON_IDEMPOTENT (ADR-006), and whether recovery can
// retry safely or must reconcile.
type Simulator struct {
	mu sync.Mutex

	SupportsIdempotency bool

	bookings []*Booking
	byKey    map[string]*Booking
	nextID   int

	// dropFor holds idempotency keys whose next response will be dropped after
	// the booking is committed. This is the fault injected in the flagship
	// experiment.
	dropFor map[string]bool

	// dropAll drops the response for every call. Used by the naive-baseline
	// demonstration.
	dropAll bool

	// rejectFor holds keys that are definitively rejected, creating nothing.
	rejectFor map[string]bool

	// requestLog records every call that reached the service, including those
	// whose response was dropped. The test oracle: it is the ground truth about
	// what the outside world actually saw.
	requestLog []LoggedRequest
}

// LoggedRequest is one call that reached the service.
type LoggedRequest struct {
	IdempotencyKey string
	Request        Request
	At             time.Time
	CreatedBooking bool
	ResponseLost   bool
}

// New returns a simulator. supportsIdempotency controls whether repeat calls
// with the same key collapse to one booking.
func New(supportsIdempotency bool) *Simulator {
	return &Simulator{
		SupportsIdempotency: supportsIdempotency,
		byKey:               map[string]*Booking{},
		dropFor:             map[string]bool{},
		rejectFor:           map[string]bool{},
	}
}

// DropResponseFor arranges that the next call with this key commits its booking
// and then fails to return it.
func (s *Simulator) DropResponseFor(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropFor[key] = true
}

// DropAllResponses makes every call commit and then lose its response.
func (s *Simulator) DropAllResponses(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropAll = on
}

// RejectFor arranges a definite rejection: no booking is created.
func (s *Simulator) RejectFor(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rejectFor[key] = true
}

// Create books a carrier.
//
// Ordering is deliberate and mirrors a real service: the booking is committed
// to durable state BEFORE the response is produced. A dropped response
// therefore leaves a real booking behind, exactly as it would in production.
func (s *Simulator) Create(ctx context.Context, key string, req Request) (*Booking, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rejectFor[key] {
		delete(s.rejectFor, key)
		s.requestLog = append(s.requestLog, LoggedRequest{
			IdempotencyKey: key, Request: req, At: time.Now(),
		})
		return nil, ErrRejected
	}

	// Idempotent replay: a key we have seen returns the original booking and
	// creates nothing new. This is what makes retry safe.
	if s.SupportsIdempotency && key != "" {
		if existing, ok := s.byKey[key]; ok {
			s.requestLog = append(s.requestLog, LoggedRequest{
				IdempotencyKey: key, Request: req, At: time.Now(),
			})
			return existing, nil
		}
	}

	s.nextID++
	b := &Booking{
		ID:             fmt.Sprintf("R%03d", s.nextID),
		IdempotencyKey: key,
		Request:        req,
		CreatedAt:      time.Now().UTC(),
	}
	s.bookings = append(s.bookings, b)
	if s.SupportsIdempotency && key != "" {
		s.byKey[key] = b
	}

	// The commit has happened. Only now can the response be lost.
	lost := s.dropAll || s.dropFor[key]
	if s.dropFor[key] {
		delete(s.dropFor, key)
	}

	s.requestLog = append(s.requestLog, LoggedRequest{
		IdempotencyKey: key,
		Request:        req,
		At:             time.Now(),
		CreatedBooking: true,
		ResponseLost:   lost,
	})

	if lost {
		return nil, ErrResponseDropped
	}
	return b, nil
}

// FindByKey is the reconciliation API: it answers "did my operation happen?"
//
// A service offering this turns an UNKNOWN effect into a knowable one. A
// service without it leaves reconciliation impossible and forces escalation to
// a human — failure F7. Whether this method exists is the difference between a
// recoverable and an unrecoverable design.
func (s *Simulator) FindByKey(key string) (*Booking, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.byKey[key]
	return b, ok
}

// Count returns the number of bookings created. The headline correctness
// metric: after a crash-and-recover run it must be exactly 1.
func (s *Simulator) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bookings)
}

// Bookings returns a copy of every booking created.
func (s *Simulator) Bookings() []Booking {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Booking, 0, len(s.bookings))
	for _, b := range s.bookings {
		out = append(out, *b)
	}
	return out
}

// Requests returns every call that reached the service, including those whose
// response was lost.
func (s *Simulator) Requests() []LoggedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]LoggedRequest, len(s.requestLog))
	copy(out, s.requestLog)
	return out
}

// DistinctKeys returns the number of distinct idempotency keys the service saw.
//
// Invariant I9's oracle. If one logical booking produced more than one key, the
// key derivation is broken and duplicates follow immediately.
func (s *Simulator) DistinctKeys() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, r := range s.requestLog {
		seen[r.IdempotencyKey] = true
	}
	return len(seen)
}
