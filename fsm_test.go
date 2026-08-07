package main

import "testing"

// fsmCase is one row of the machine's transition table.
type fsmCase struct {
	from    deliveryState
	effect  effect
	facts   subjectFacts
	to      deliveryState
	wantErr bool
}

// TestTransitLegal walks the happy paths the three flows actually take. Each row
// must land exactly where docs/fsm.md says, or the machine has drifted from the
// flows that are its source of truth.
func TestTransitLegal(t *testing.T) {
	cases := []fsmCase{
		// Builder: eligible item -> building -> published branch.
		{from: stateEligible, effect: effectBuild, to: stateBuilding},
		{from: stateBuilding, effect: effectPublish, to: statePushed},
		// Verifier: published branch -> Checks note.
		{from: statePushed, effect: effectCheck,
			facts: subjectFacts{checksStatus: "pass"}, to: stateChecksRecorded},
		// Verifier: green Checks -> approving Verdict.
		{from: stateChecksRecorded, effect: effectReview,
			facts: subjectFacts{checksStatus: "pass", verdictStatus: "approve"},
			to:    stateVerdictApproved},
		// Verifier: green Checks -> rejecting Verdict.
		{from: stateChecksRecorded, effect: effectReview,
			facts: subjectFacts{checksStatus: "pass", verdictStatus: "changes"},
			to:    stateVerdictRejected},
		// Fixer: failed Checks -> repair; a repair is a publish to a bare head.
		{from: stateChecksRecorded, effect: effectFix,
			facts: subjectFacts{checksStatus: "fail"}, to: stateFixing},
		{from: stateFixing, effect: effectPublish, to: statePushed},
		// Fixer: rejected Verdict -> repair.
		{from: stateVerdictRejected, effect: effectFix, to: stateFixing},
		// Verifier: approved + green -> merge. This is the only path to merged.
		{from: stateVerdictApproved, effect: effectMerge,
			facts: subjectFacts{checksStatus: "pass", verdictStatus: "approve"},
			to:    stateMerged},
		// Fixer attempts exhausted -> halted for a human.
		{from: stateVerdictRejected, effect: effectFail,
			facts: subjectFacts{attempts: 2, attemptsCap: 2}, to: stateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.from.String()+"->"+tc.effect.String(), func(t *testing.T) {
			to, err := transit(tc.from, tc.effect, tc.facts)
			if err != nil {
				t.Fatalf("transit(%s, %s) = error %v, want %s", tc.from, tc.effect, err, tc.to)
			}
			if to != tc.to {
				t.Fatalf("transit(%s, %s) = %s, want %s", tc.from, tc.effect, to, tc.to)
			}
		})
	}
}

// TestTransitIllegal pins the machine's refusals. Each row names a move a flow
// may not make; if a flow ever tries one, the test fails and the machine has
// stopped enforcing a transition.
func TestTransitIllegal(t *testing.T) {
	cases := []fsmCase{
		// Never double-build a subject another flow already claimed: once an
		// item is building, no second build may start.
		{from: stateBuilding, effect: effectBuild},
		{from: stateFixing, effect: effectFix},
		// A builder only starts from an eligible item; a branch already exists
		// here and the item was already claimed.
		{from: statePushed, effect: effectBuild},
		{from: stateChecksRecorded, effect: effectBuild},
		// Nothing checks or reviews a subject that has no branch yet.
		{from: stateEligible, effect: effectCheck},
		{from: stateEligible, effect: effectReview},
		// A Verdict may not be written before the Checks note on the exact
		// revision: review requires green Checks.
		{from: statePushed, effect: effectReview,
			facts: subjectFacts{verdictStatus: "approve"}},
		{from: stateChecksRecorded, effect: effectReview,
			facts: subjectFacts{checksStatus: "fail"}},
		// A failing head is never reviewed and an approved head is never fixed.
		{from: stateChecksRecorded, effect: effectFix,
			facts: subjectFacts{checksStatus: "pass"}},
		{from: stateVerdictApproved, effect: effectFix},
		// Never merge an unapproved or unverified head.
		{from: statePushed, effect: effectMerge},
		{from: stateChecksRecorded, effect: effectMerge,
			facts: subjectFacts{checksStatus: "pass"}},
		{from: stateVerdictRejected, effect: effectMerge,
			facts: subjectFacts{checksStatus: "pass"}},
		// Never merge without green Checks on the exact approved revision.
		{from: stateVerdictApproved, effect: effectMerge,
			facts: subjectFacts{checksStatus: "fail"}},
		// Terminal states accept no further move.
		{from: stateMerged, effect: effectPublish},
		{from: stateMerged, effect: effectMerge},
		{from: stateFailed, effect: effectFix},
		{from: stateFailed, effect: effectFail},
	}
	for _, tc := range cases {
		t.Run(tc.from.String()+"->"+tc.effect.String(), func(t *testing.T) {
			if _, err := transit(tc.from, tc.effect, tc.facts); err == nil {
				t.Fatalf("transit(%s, %s) = legal, want illegal transition refused",
					tc.from, tc.effect)
			}
		})
	}
}

// TestEffectOwners pins which Flow owns each effect, so the machine and the two
// agent declarations agree about who may act.
func TestEffectOwners(t *testing.T) {
	owners := map[effect]string{
		effectBuild: "builder", effectPublish: "builder",
		effectCheck: "verifier", effectReview: "verifier", effectMerge: "verifier",
		effectFix: "fixer", effectFail: "human",
	}
	for e, want := range owners {
		if got := flowName(e); got != want {
			t.Fatalf("flowName(%s) = %s, want %s", e, got, want)
		}
	}
}

// TestObserveDerivesStatesFromFacts pins that every durable state comes from
// git-visible facts alone and that a new commit never inherits a Verdict or
// Checks note.
func TestObserveDerivesStatesFromFacts(t *testing.T) {
	cases := []struct {
		name string
		fsm  subjectFacts
		want deliveryState
	}{
		{"eligible open item, no branch", subjectFacts{itemOpen: true}, stateEligible},
		{"closed item, no branch is merged", subjectFacts{}, stateMerged},
		{"bare branch", subjectFacts{hasBranch: true, itemOpen: true}, statePushed},
		{"checks recorded", subjectFacts{hasBranch: true, itemOpen: true, checksStatus: "pass"}, stateChecksRecorded},
		{"approved", subjectFacts{hasBranch: true, itemOpen: true, checksStatus: "pass", verdictStatus: "approve"}, stateVerdictApproved},
		{"rejected", subjectFacts{hasBranch: true, itemOpen: true, checksStatus: "pass", verdictStatus: "changes"}, stateVerdictRejected},
		{"exhausted attempts", subjectFacts{hasBranch: true, itemOpen: true, checksStatus: "fail", attempts: 2, attemptsCap: 2}, stateFailed},
		{"forest:failed label", subjectFacts{hasBranch: true, itemOpen: true, failedLabel: true}, stateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := observe(tc.fsm); got != tc.want {
				t.Fatalf("observe(%+v) = %s, want %s", tc.fsm, got, tc.want)
			}
		})
	}
}
