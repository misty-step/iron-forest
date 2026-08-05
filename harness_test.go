package main

import (
	"testing"
	"time"
)

// TestPhaseContextUnboundedWithoutBudget pins the no-deadline contract. An agent
// reasoning at maximum effort can work for a long time and still be productive,
// so an undeclared budget must produce a context with no deadline: a clock here
// kills a working run and throws away every token it already spent.
func TestPhaseContextUnboundedWithoutBudget(t *testing.T) {
	for _, budget := range []time.Duration{0, -time.Second} {
		ctx, cancel := phaseContext(budget)
		if deadline, ok := ctx.Deadline(); ok {
			t.Errorf("budget %v produced a deadline at %v", budget, deadline)
		}
		cancel()
	}
}

// TestPhaseContextHonorsDeclaredBudget keeps the opt-in bound working: an agent
// that declares a budget is still stopped at it.
func TestPhaseContextHonorsDeclaredBudget(t *testing.T) {
	ctx, cancel := phaseContext(30 * time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("declared budget produced no deadline")
	}
	if left := time.Until(deadline); left <= 0 || left > 30*time.Second {
		t.Fatalf("deadline in %v, want inside 30s", left)
	}
}
