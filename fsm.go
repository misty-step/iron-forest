package main

import (
	"fmt"
)

// This file models the delivery state machine documented in docs/fsm.md. The
// machine is the single authority on what a flow may do, and it is derived only
// from git-visible facts: a forest branch, a Verdict note, a Checks note, the
// attempt ref, and the tracker's forest:failed label. encoding the machine as a
// pure function means an illegal move fails its test wherever a flow would try
// it, instead of relying on each selector to remember the same rule.

// deliveryState is one place a subject can sit in the delivery machine.
type deliveryState int

const (
	stateEligible deliveryState = iota
	stateBuilding
	statePushed
	stateChecksRecorded
	stateVerdictApproved
	stateVerdictRejected
	stateFixing
	stateMerged
	stateFailed
)

var stateNames = [...]string{
	"eligible", "building", "pushed", "checks_recorded",
	"verdict_approved", "verdict_rejected", "fixing", "merged", "failed",
}

func (s deliveryState) String() string {
	if s < 0 || int(s) >= len(stateNames) {
		return fmt.Sprintf("state(%d)", s)
	}
	return stateNames[s]
}

// effect is one durable move a flow performs on a subject.
type effect int

const (
	effectBuild   effect = iota // Builder runs the agent in a worktree
	effectPublish               // Builder/Fixer publishes a branch head
	effectCheck                 // Verifier writes a Checks note
	effectReview                // Verifier writes a Verdict note
	effectFix                   // Fixer repairs a broken head
	effectMerge                 // Verifier lands an approved head
	effectFail                  // Fixer or human marks the subject halted
)

var effectNames = [...]string{
	"build", "publish", "check", "review", "fix", "merge", "fail",
}

func (e effect) String() string {
	if e < 0 || int(e) >= len(effectNames) {
		return fmt.Sprintf("effect(%d)", e)
	}
	return effectNames[e]
}

// flowName reports one owner Flow for an effect, for the transition table and
// for the docs. It is a pure mapping, never a running process. publish is
// attributed to the Builder because the Fixer runs the same Builder
// declaration; owns is the permissive permission check `transit` actually uses.
func flowName(e effect) string {
	switch e {
	case effectBuild, effectPublish:
		return "builder" // the Fixer runs the same Builder declaration
	case effectCheck, effectReview, effectMerge:
		return "verifier"
	case effectFix:
		return "fixer"
	case effectFail:
		return "human" // halt states are an operator decision
	}
	return "unknown"
}

// owns reports whether a named actor may perform an effect. It is the
// ownership half of the machine: one lane can never claim another lane's
// decision, so the Fixer's repairs (destroying a broken head and publishing a
// fresh one) are distinct from the Builder's original claim, and the attempts-
// exhausted halt is a Fixer action, not just a human one.
func owns(e effect, actor string) bool {
	switch e {
	case effectBuild:
		return actor == "builder"
	case effectPublish:
		return actor == "builder" || actor == "fixer"
	case effectCheck, effectReview, effectMerge:
		return actor == "verifier"
	case effectFix:
		return actor == "fixer"
	case effectFail:
		return actor == "human" || actor == "fixer"
	}
	return false
}

// subjectFacts are the git-visible facts that place one subject in the machine.
// Every note in the set keys to the same exact Revision (the head commit), which
// is what makes the exact-note invariant checkable: a decision that merges, or
// admits a review, must carry the Revision it was recorded against.
type subjectFacts struct {
	revision      string // the exact Revision these facts describe (a head sha)
	hasBranch     bool   // a forest/<id>-<slug> branch exists on origin
	itemOpen      bool   // the tracker item is still open
	checksStatus  string // "" | pass | fail on revision
	verdictStatus string // "" | approve | changes on revision
	attempts      int    // fix attempts already spent
	attemptsCap   int    // configured fixer.attempts
	failedLabel   bool   // tracker carries forest:failed
}

// observe derives the durable resting state from the git-visible facts. The
// transient states building and fixing are not git-visible: they exist only
// while a run holds the subject in-process, so observe never reports them. For
// the same reason a new commit can never inherit a Verdict or Checks note:
// every publish returns a subject to pushed, where no note exists.
func observe(f subjectFacts) deliveryState {
	if f.failedLabel || (f.attemptsCap > 0 && f.attempts >= f.attemptsCap) {
		return stateFailed
	}
	if !f.hasBranch {
		if !f.itemOpen {
			return stateMerged
		}
		return stateEligible
	}
	// A failing head is repair work, never a review outcome, and a Verdict is
	// only ever *written* on a green head. The Checks fact therefore dominates
	// the Verdict fact: a head whose Checks fail is observed as checks_recorded
	// (the Fixer's work) even if a stray fact still names a Verdict. The green
	// Checks fact must likewise be present before a Verdict is read as a state:
	// a Verdict with no green Checks on the exact revision is a stranded signal,
	// never an approved or rejected outcome that a lane may act on.
	if f.checksStatus == "fail" {
		return stateChecksRecorded
	}
	switch {
	case f.checksStatus == "pass" && f.verdictStatus == "approve":
		return stateVerdictApproved
	case f.checksStatus == "pass" && f.verdictStatus == "changes":
		return stateVerdictRejected
	case f.checksStatus != "":
		return stateChecksRecorded
	default:
		return statePushed
	}
}

// transit advances a subject through one effect performed by actor and returns
// the next state, or an error naming the illegal move. It is the single
// authority on what a flow may do. outcome is the effect's own recorded
// decision (the Verdict "approve" or "changes" for a review; empty otherwise),
// so a review does not need the Verdict to pre-exist before the effect that
// writes it. An effect a flow may not perform from the subject's state, one the
// wrong actor attempts, or one a fact forbids -- reviewing a head whose Checks
// are not green, or merging without Checks and an approved Verdict on the exact
// revision -- is refused here.
func transit(from deliveryState, e effect, f subjectFacts, outcome, actor string) (deliveryState, error) {
	// Ownership: a lane may only perform an effect it owns. actor is "" when a
	// caller asks about legality without naming a running flow, which skips the
	// check so the transition table can be pinned in tests.
	if actor != "" && !owns(e, actor) {
		return stateFailed, fmt.Errorf("illegal transition: %s cannot perform %s (owned by %s)",
			actor, e, flowName(e))
	}
	// Exact-revision: a decision that writes a Verdict, or admits a merge, only
	// means something when it keys to the exact Revision it records. A fact set
	// that carries no Revision cannot satisfy that invariant.
	if (e == effectReview || e == effectMerge) && f.revision == "" {
		return stateFailed, fmt.Errorf("illegal transition %s --%s--> ?: no exact revision", from, e)
	}
	switch e {
	case effectBuild:
		if from == stateEligible {
			return stateBuilding, nil
		}
	case effectPublish:
		// A publish always lands a head with no note, so building and fixing
		// both return the subject to pushed.
		if from == stateBuilding || from == stateFixing {
			return statePushed, nil
		}
	case effectCheck:
		if from == statePushed {
			return stateChecksRecorded, nil
		}
	case effectReview:
		// The review effect writes the Verdict; outcome names it. Legality
		// depends only on what must already be true -- a green Checks note on
		// the exact revision -- never on a Verdict that does not exist yet.
		if from == stateChecksRecorded && f.checksStatus == "pass" {
			switch outcome {
			case "approve":
				return stateVerdictApproved, nil
			case "changes":
				return stateVerdictRejected, nil
			}
		}
	case effectFix:
		// A head is fix work only when it is broken: failed Checks, or a Verdict
		// of changes. A green, approved head belongs to the merge path, never to
		// the Fixer. A rejected head necessarily has green Checks, because the
		// Verdict is only ever written on a Checks-pass head.
		if from == stateChecksRecorded && f.checksStatus == "fail" ||
			from == stateVerdictRejected {
			return stateFixing, nil
		}
	case effectMerge:
		// Invariant: never merge without Checks and an approved Verdict on the
		// exact revision. Only an approved head whose Checks pass may land.
		if from == stateVerdictApproved && f.checksStatus == "pass" {
			return stateMerged, nil
		}
	case effectFail:
		// failed and merged are terminal. A Fixer halts a subject only once its
		// repair attempts are exhausted: a fresh subject -- one whose attempts
		// are below the cap -- cannot be marked failed by the machine, so the
		// FSM API cannot label work that still has attempts left. A subject that
		// observe already reports as failed (attempts at the cap) is exactly
		// that exhaustion, so the Fixer's halt is the one legal move out of it.
		// An operator may halt any working subject for review.
		if from == stateMerged {
			break
		}
		if actor == "fixer" {
			if !(f.attemptsCap > 0 && f.attempts >= f.attemptsCap) {
				break
			}
			return stateFailed, nil
		}
		if from == stateFailed {
			break
		}
		return stateFailed, nil
	}
	return stateFailed, fmt.Errorf("illegal transition %s --%s--> ?", from, e)
}
