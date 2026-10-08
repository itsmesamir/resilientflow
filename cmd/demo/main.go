// Command demo runs the lost-response scenario under two recovery strategies
// and prints what the outside world ended up holding.
//
// Phase 1 only: everything is in one process with an in-memory journal, so
// "durable resume" here is a preview of what Phase 2 makes real rather than a
// property this build actually has. The point is to make the failure visible.
//
//	go run ./cmd/demo
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/itsmesamir/resilientflow/adapters/model/fake"
	"github.com/itsmesamir/resilientflow/adapters/tools/booking"
	"github.com/itsmesamir/resilientflow/benchmark/workloads"
	"github.com/itsmesamir/resilientflow/internal/workflow"
)

type scenario struct {
	name        string
	description string
	// resumeRunID is the run identity used after the crash. Reusing the
	// original is what durable recovery can do and restart-from-zero cannot.
	resumeRunID string
	journal     func(previous workflow.Journal) workflow.Journal
}

func main() {
	input := workloads.FreightInput{Origin: "SEA", Dest: "PDX", WeightKg: 250}

	scenarios := []scenario{
		{
			name:        "Baseline A — restart from zero",
			description: "nothing survived the crash, so the retry is a new run with new keys",
			resumeRunID: "run-2",
			journal:     func(workflow.Journal) workflow.Journal { return workflow.NewMemoryJournal() },
		},
		{
			name:        "Durable resume — same run, same keys",
			description: "the history survived, so the retry carries the original idempotency key",
			resumeRunID: "run-1",
			journal:     func(prev workflow.Journal) workflow.Journal { return prev },
		},
	}

	fmt.Println("ResilientFlow — the lost response")
	fmt.Println("=================================")
	fmt.Println()
	fmt.Println("A freight workflow books a carrier. The booking service commits the")
	fmt.Println("reservation and the response is lost before it reaches the worker.")
	fmt.Println("The worker cannot tell whether the booking exists.")
	fmt.Println()

	failures := 0

	for _, sc := range scenarios {
		m := fake.NewDeterministic()
		sim := booking.New(true)
		freight := &workloads.Freight{Model: m, Booking: sim, BookingClass: workflow.Idempotent}

		fmt.Printf("── %s\n", sc.name)
		fmt.Printf("   %s\n\n", sc.description)

		// Attempt 1: the response is dropped after the booking commits.
		sim.DropAllResponses(true)
		j1 := workflow.NewMemoryJournal()
		first, _ := workflow.Run(context.Background(), freight.Definition(), "run-1", input, j1, 1)

		fmt.Printf("   attempt 1 → state=%s\n", first.State)
		fmt.Printf("              bookings in the external system: %d\n", sim.Count())
		fmt.Printf("              the effect happened; the worker does not know it\n\n")

		// Attempt 2: recovery.
		sim.DropAllResponses(false)
		modelCallsBefore := m.Calls()
		second, _ := workflow.Run(context.Background(), freight.Definition(),
			sc.resumeRunID, input, sc.journal(j1), 1)

		fmt.Printf("   attempt 2 → state=%s  executed=%d replayed=%d\n",
			second.State, second.Executed, second.Replayed)
		fmt.Printf("              model calls this attempt: %d\n", m.Calls()-modelCallsBefore)
		fmt.Printf("              distinct idempotency keys seen by the service: %d\n", sim.DistinctKeys())

		count := sim.Count()
		verdict := "✓ one booking"
		if count != 1 {
			verdict = fmt.Sprintf("✗ %d BOOKINGS — the customer was charged twice", count)
			failures++
		}
		fmt.Printf("              bookings in the external system: %d  %s\n\n", count, verdict)

		for _, b := range sim.Bookings() {
			fmt.Printf("              %s  carrier=%s  $%d  key=%s…\n",
				b.ID, b.Request.CarrierID, b.Request.PriceUSD, b.IdempotencyKey[:8])
		}
		fmt.Println()
	}

	fmt.Println("What changed between the two runs:")
	fmt.Println("  nothing about the booking service, the network, or the fault.")
	fmt.Println("  Only whether the retry could derive the same idempotency key")
	fmt.Println("  as the original attempt — which requires the history to survive.")
	fmt.Println()
	fmt.Println("  See docs/design-decisions.md ADR-003.")

	if failures != 1 {
		fmt.Fprintf(os.Stderr, "\nunexpected: %d scenarios duplicated, want exactly 1\n", failures)
		os.Exit(1)
	}
}
