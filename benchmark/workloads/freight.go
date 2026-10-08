// Package workloads holds the workflows the benchmark runs.
//
// The freight booking workflow is the representative workload: it contains a
// cheap deterministic prefix, an expensive nondeterministic step, and a real
// external side effect, which is the combination the project exists to study.
// Synthetic workflows are added alongside it, because benchmarking only the
// demo would prove nothing general.
package workloads

import (
	"context"
	"fmt"
	"sort"

	"github.com/itsmesamir/resilientflow/adapters/model"
	"github.com/itsmesamir/resilientflow/adapters/tools/booking"
	"github.com/itsmesamir/resilientflow/internal/workflow"
)

// FreightInput is a shipment to book.
type FreightInput struct {
	Origin   string `json:"origin"`
	Dest     string `json:"dest"`
	WeightKg int    `json:"weight_kg"`
}

// FreightOutput is the completed booking.
type FreightOutput struct {
	BookingID string `json:"booking_id"`
	CarrierID string `json:"carrier_id"`
	PriceUSD  int    `json:"price_usd"`
}

// Carrier is a candidate haulier.
type Carrier struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RateCent int    `json:"rate_cent"`
	Rating   int    `json:"rating"`
}

// Quote is a price for a specific shipment.
type Quote struct {
	CarrierID string `json:"carrier_id"`
	PriceUSD  int    `json:"price_usd"`
}

// Freight builds the freight booking workflow against injected dependencies.
//
// BookingClass is supplied by the caller because it is a property of the
// provider, not of the workflow (ADR-006): the same code is IDEMPOTENT against
// a service that accepts keys and NON_IDEMPOTENT against one that does not.
// Running the same workflow under both is how the benchmark isolates what
// idempotency support is worth.
type Freight struct {
	Model        model.Model
	Booking      *booking.Simulator
	BookingClass workflow.Class
}

// Definition returns the workflow.
//
// The function obeys the determinism contract: no clock reads, no randomness,
// no I/O outside Execute, and the sort is total so iteration order is stable.
func (f *Freight) Definition() workflow.Definition[FreightInput, FreightOutput] {
	return workflow.Definition[FreightInput, FreightOutput]{
		Name: "freight_booking",
		Fn: func(wctx *workflow.Context, in FreightInput) (FreightOutput, error) {
			var zero FreightOutput

			// 1. Retrieve candidates. Local computation, no effect.
			carriers, err := workflow.Execute(wctx, f.retrieveCarriers(), in)
			if err != nil {
				return zero, fmt.Errorf("retrieve_carriers: %w", err)
			}

			// 2. Rank them. Pure function of the input.
			ranked, err := workflow.Execute(wctx, f.rankCarriers(), carriers)
			if err != nil {
				return zero, fmt.Errorf("rank_carriers: %w", err)
			}

			// 3. Request quotes. Reads external state; no effect, but the
			//    answer may differ between calls.
			quotes, err := workflow.Execute(wctx, f.requestQuotes(in), ranked)
			if err != nil {
				return zero, fmt.Errorf("request_quotes: %w", err)
			}

			// 4. Negotiate. Expensive and nondeterministic: recovery reuses the
			//    recorded answer rather than paying for it twice.
			choice, err := workflow.Execute(wctx, f.negotiate(), quotes)
			if err != nil {
				return zero, fmt.Errorf("llm_negotiate: %w", err)
			}

			// 5. Create the booking. The external side effect, and the only
			//    step in this workflow where a crash can cost real money.
			result, err := workflow.Execute(wctx, f.createBooking(in), choice)
			if err != nil {
				return zero, fmt.Errorf("create_booking: %w", err)
			}

			// 6. Notify operations. Idempotent via a stable notification ID.
			if _, err := workflow.Execute(wctx, f.notify(), result); err != nil {
				return zero, fmt.Errorf("notify_ops: %w", err)
			}

			return result, nil
		},
	}
}

// --- activities ------------------------------------------------------------

func (f *Freight) retrieveCarriers() workflow.Activity[FreightInput, []Carrier] {
	return workflow.Activity[FreightInput, []Carrier]{
		Name:  "retrieve_carriers",
		Class: workflow.Pure,
		Fn: func(_ context.Context, _ workflow.Call, in FreightInput) ([]Carrier, error) {
			return []Carrier{
				{ID: "C-001", Name: "Northwind Freight", RateCent: 180, Rating: 4},
				{ID: "C-002", Name: "Blue Ridge Haulage", RateCent: 165, Rating: 3},
				{ID: "C-003", Name: "Cascade Logistics", RateCent: 210, Rating: 5},
			}, nil
		},
	}
}

func (f *Freight) rankCarriers() workflow.Activity[[]Carrier, []Carrier] {
	return workflow.Activity[[]Carrier, []Carrier]{
		Name:  "rank_carriers",
		Class: workflow.Pure,
		Fn: func(_ context.Context, _ workflow.Call, cs []Carrier) ([]Carrier, error) {
			out := make([]Carrier, len(cs))
			copy(out, cs)
			// Total order: rating descending, then rate ascending, then ID.
			// Ties must break deterministically or replay diverges.
			sort.Slice(out, func(i, j int) bool {
				if out[i].Rating != out[j].Rating {
					return out[i].Rating > out[j].Rating
				}
				if out[i].RateCent != out[j].RateCent {
					return out[i].RateCent < out[j].RateCent
				}
				return out[i].ID < out[j].ID
			})
			return out, nil
		},
	}
}

func (f *Freight) requestQuotes(in FreightInput) workflow.Activity[[]Carrier, []Quote] {
	return workflow.Activity[[]Carrier, []Quote]{
		Name:  "request_quotes",
		Class: workflow.ReadOnly,
		Fn: func(_ context.Context, _ workflow.Call, cs []Carrier) ([]Quote, error) {
			qs := make([]Quote, 0, len(cs))
			for _, c := range cs {
				qs = append(qs, Quote{
					CarrierID: c.ID,
					PriceUSD:  (c.RateCent * in.WeightKg) / 100,
				})
			}
			return qs, nil
		},
	}
}

func (f *Freight) negotiate() workflow.Activity[[]Quote, Quote] {
	return workflow.Activity[[]Quote, Quote]{
		Name:  "llm_negotiate",
		Class: workflow.NondeterministicExpensive,
		Fn: func(ctx context.Context, _ workflow.Call, qs []Quote) (Quote, error) {
			if len(qs) == 0 {
				return Quote{}, fmt.Errorf("no quotes to negotiate")
			}
			prompt := fmt.Sprintf("negotiate best of %d quotes, leader %s at $%d",
				len(qs), qs[0].CarrierID, qs[0].PriceUSD)

			resp, err := f.Model.Generate(ctx, model.Request{Prompt: prompt, MaxTokens: 64})
			if err != nil {
				return Quote{}, err
			}

			// The model "negotiates" a discount. The point is not the
			// arithmetic; it is that this step is expensive and its result must
			// survive recovery without being recomputed.
			best := qs[0]
			best.PriceUSD = best.PriceUSD - (len(resp.Text) % 50)
			return best, nil
		},
	}
}

func (f *Freight) createBooking(in FreightInput) workflow.Activity[Quote, FreightOutput] {
	return workflow.Activity[Quote, FreightOutput]{
		Name:  "create_booking",
		Class: f.BookingClass,
		Fn: func(ctx context.Context, call workflow.Call, q Quote) (FreightOutput, error) {
			// The idempotency key the runtime derived travels with the request.
			// Against a provider that honours it, a retry is free; against one
			// that does not, this call duplicates and recovery must reconcile.
			b, err := f.Booking.Create(ctx, call.IdempotencyKey, booking.Request{
				CarrierID: q.CarrierID,
				Origin:    in.Origin,
				Dest:      in.Dest,
				PriceUSD:  q.PriceUSD,
			})
			if err != nil {
				return FreightOutput{}, err
			}
			return FreightOutput{
				BookingID: b.ID,
				CarrierID: q.CarrierID,
				PriceUSD:  q.PriceUSD,
			}, nil
		},
	}
}

func (f *Freight) notify() workflow.Activity[FreightOutput, string] {
	return workflow.Activity[FreightOutput, string]{
		Name:  "notify_ops",
		Class: workflow.Idempotent,
		Fn: func(_ context.Context, call workflow.Call, out FreightOutput) (string, error) {
			// The key doubles as the notification ID, which is what makes a
			// repeated notification collapse rather than spam the operator.
			return "notified:" + out.BookingID + ":" + call.IdempotencyKey, nil
		},
	}
}
