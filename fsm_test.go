package main

import "testing"

// fsmCase is one row of the machine's transition table. outcome is the effect's
// own recorded decision (the Verdict for a review, empty otherwise); actor is
// the Flow performing the effect ("" when the row only pins the table).
type fsmCase struct {
	from    deliveryState
	effect  effect
	facts   subjectFacts
	outcome string
	actor   string
	to      deliveryState
	wantErr bool
}

func rev(r string) subjectFacts { return subjectFacts{revision: r} }

// TestTransitLegal walks the happy paths the three flows actually take. Each row
// must land exactly where docs/fsm.md says, or the machine has drifted from the
// flows that are its source of truth.
func TestTransitLegal(t *testing.T) {
	cases := []fsmCase{
		// Builder: eligible item -> building -> published branch.
		{from: stateEligible, effect: effectBuild, facts: rev("i"), actor: "builder", to: stateBuilding},
		{from: stateBuilding, effect: effectPublish, facts: rev("h"), actor: "builder", to: statePushed},
		// Verifier: published branch -> Checks note.
		{from: statePushed, effect: effectCheck, facts: rev("h"), actor: "verifier", to: stateChecksRecorded},
		// Verifier: green Checks -> approving Verdict. The review effect writes
		// the Verdict; it does not need one to pre-exist.
		{from: stateChecksRecorded, effect: effectReview,
			facts: subjectFacts{revision: "h", checksStatus: "pass"}, outcome: "approve", actor: "verifier", to: stateVerdictApproved},
		// Verifier: green Checks -> rejecting Verdict.
		{from: stateChecksRecorded, effect: effectReview,
			facts: subjectFacts{revision: "h", checksStatus: "pass"}, outcome: "changes", actor: "verifier", to: stateVerdictRejected},
		// Fixer: failed Checks -> repair; a repair is a publish to a bare head.
		{from: stateChecksRecorded, effect: effectFix,
			facts: subjectFacts{revision: "h", checksStatus: "fail"}, actor: "fixer", to: stateFixing},
		{from: stateFixing, effect: effectPublish, facts: rev("h"), actor: "fixer", to: statePushed},
		// Fixer: rejected Verdict -> repair.
		{from: stateVerdictRejected, effect: effectFix, facts: rev("h"), actor: "fixer", to: stateFixing},
		// Verifier: approved + green -> merge. This is the only path to merged.
		{from: stateVerdictApproved, effect: effectMerge,
			facts: subjectFacts{revision: "h", checksStatus: "pass", verdictStatus: "approve"}, actor: "verifier", to: stateMerged},
		// Fixer attempts exhausted -> halted for a human.
		{from: stateVerdictRejected, effect: effectFail,
			facts: subjectFacts{revision: "h", attempts: 2, attemptsCap: 2}, actor: "fixer", to: stateFailed},
		// The effectFail halt is also an operator decision.
		{from: statePushed, effect: effectFail, facts: rev("h"), actor: "human", to: stateFailed},
	}
	for _, tc := range cases {
		t.Run(tc.from.String()+"->"+tc.effect.String(), func(t *testing.T) {
			to, err := transit(tc.from, tc.effect, tc.facts, tc.outcome, tc.actor)
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
		{from: stateBuilding, effect: effectBuild, actor: "builder"},
		{from: stateFixing, effect: effectFix, actor: "fixer"},
		// A builder only starts from an eligible item; a branch already exists
		// here and the item was already claimed.
		{from: statePushed, effect: effectBuild, actor: "builder"},
		{from: stateChecksRecorded, effect: effectBuild, actor: "builder"},
		// Nothing checks or reviews a subject that has no branch yet.
		{from: stateEligible, effect: effectCheck, actor: "verifier"},
		{from: stateEligible, effect: effectReview, facts: rev("h"), outcome: "approve", actor: "verifier"},
		// A Verdict may not be written before the Checks note on the exact
		// revision: review requires green Checks.
		{from: statePushed, effect: effectReview,
			facts: rev("h"), outcome: "approve", actor: "verifier"},
		{from: stateChecksRecorded, effect: effectReview,
			facts: subjectFacts{revision: "h", checksStatus: "fail"}, outcome: "approve", actor: "verifier"},
		// A review must name its Verdict; an outcome-less review is a no-op.
		{from: stateChecksRecorded, effect: effectReview,
			facts: subjectFacts{revision: "h", checksStatus: "pass"}, actor: "verifier"},
		// A failing head is never reviewed and an approved head is never fixed.
		{from: stateChecksRecorded, effect: effectFix,
			facts: subjectFacts{revision: "h", checksStatus: "pass"}, actor: "fixer"},
		{from: stateVerdictApproved, effect: effectFix, actor: "fixer"},
		// Never merge an unapproved or unverified head.
		{from: statePushed, effect: effectMerge, facts: rev("h"), actor: "verifier"},
		{from: stateChecksRecorded, effect: effectMerge,
			facts: subjectFacts{revision: "h", checksStatus: "pass"}, actor: "verifier"},
		{from: stateVerdictRejected, effect: effectMerge,
			facts: subjectFacts{revision: "h", checksStatus: "pass"}, actor: "verifier"},
		// Never merge without green Checks on the exact approved revision.
		{from: stateVerdictApproved, effect: effectMerge,
			facts: subjectFacts{revision: "h", checksStatus: "fail", verdictStatus: "approve"}, actor: "verifier"},
		// A decision that writes a Verdict or admits a merge needs the exact
		// Revision it records; an anonymous fact set cannot satisfy that.
		{from: stateChecksRecorded, effect: effectReview,
			facts: subjectFacts{checksStatus: "pass"}, outcome: "approve", actor: "verifier"},
		{from: stateVerdictApproved, effect: effectMerge,
			facts: subjectFacts{checksStatus: "pass", verdictStatus: "approve"}, actor: "verifier"},
		// Ownership: a lane that does not own an effect may not perform it.
		{from: stateEligible, effect: effectBuild, actor: "verifier"},
		{from: statePushed, effect: effectCheck, actor: "builder"},
		{from: stateChecksRecorded, effect: effectFix, actor: "verifier"},
		{from: stateFixing, effect: effectPublish, actor: "verifier"},
		// Terminal states accept no further move.
		{from: stateMerged, effect: effectPublish, actor: "builder"},
		{from: stateMerged, effect: effectMerge, actor: "verifier"},
		{from: stateFailed, effect: effectFix, actor: "fixer"},
		{from: stateFailed, effect: effectFail, actor: "human"},
	}
	for _, tc := range cases {
		t.Run(tc.from.String()+"->"+tc.effect.String(), func(t *testing.T) {
			if _, err := transit(tc.from, tc.effect, tc.facts, tc.outcome, tc.actor); err == nil {
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

// TestOwnerPermission pins the permissive side of ownership: the Fixer may
// publish its own repair (it runs the Builder declaration) and may halt a
// subject at the attempt cap, while the Builder may not fix or review.
func TestOwnerPermission(t *testing.T) {
	if !owns(effectPublish, "builder") || !owns(effectPublish, "fixer") {
		t.Fatal("publish must be owned by both builder and fixer")
	}
	if !owns(effectFix, "fixer") || owns(effectFix, "builder") {
		t.Fatal("fix must be owned only by the fixer")
	}
	if !owns(effectReview, "verifier") || owns(effectReview, "builder") {
		t.Fatal("review must be owned only by the verifier")
	}
	if !owns(effectFail, "human") || !owns(effectFail, "fixer") {
		t.Fatal("fail must be owned by the fixer and the human")
	}
	if owns(effectMerge, "builder") {
		t.Fatal("the builder must never merge")
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
		// Failing Checks dominate a Verdict: a broken head is the Fixer's work,
		// never a reviewed outcome.
		{"failing checks with approving verdict is fix work",
			subjectFacts{hasBranch: true, itemOpen: true, checksStatus: "fail", verdictStatus: "approve"}, stateChecksRecorded},
		{"failing checks with rejecting verdict is fix work",
			subjectFacts{hasBranch: true, itemOpen: true, checksStatus: "fail", verdictStatus: "changes"}, stateChecksRecorded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := observe(tc.fsm); got != tc.want {
				t.Fatalf("observe(%+v) = %s, want %s", tc.fsm, got, tc.want)
			}
		})
	}
}
