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

// flowName reports which Flow owns an effect, so the machine and the two agent
// declarations agree about who may act. It is a pure mapping, never a running
// process.
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

// subjectFacts are the git-visible facts that place one subject in the machine.
type subjectFacts struct {
	hasBranch     bool   // a forest/<id>-<slug> branch exists on origin
	itemOpen      bool   // the tracker item is still open
	checksStatus  string // "" | pass | fail on the head
	verdictStatus string // "" | approve | changes on the head
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
	switch {
	case f.verdictStatus == "approve":
		return stateVerdictApproved
	case f.verdictStatus == "changes":
		return stateVerdictRejected
	case f.checksStatus != "":
		return stateChecksRecorded
	default:
		return statePushed
	}
}

// transit advances a subject through one effect and returns the next state, or
// an error naming the illegal move. It is the single authority on what a flow
// may do: an effect a flow may not perform from the subject's state, or one a
// fact forbids -- reviewing a head whose Checks are not green, merging without
// Checks and an approved Verdict on the exact revision -- is refused here.
func transit(from deliveryState, e effect, f subjectFacts) (deliveryState, error) {
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
		if from == stateChecksRecorded && f.checksStatus == "pass" {
			if f.verdictStatus == "approve" {
				return stateVerdictApproved, nil
			}
			if f.verdictStatus == "changes" {
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
		if from != stateMerged && from != stateFailed {
			return stateFailed, nil
		}
	}
	return stateFailed, fmt.Errorf("illegal transition %s --%s--> ?", from, e)
}
