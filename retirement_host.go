package main

import (
	"errors"
	"fmt"
)

// recordPreparingHostRetirement publishes the recovery identity before any Host
// request can exist. A preparing fact blocks duplicate Builder work but still
// lets the live branch reach the Verifier.
func recordPreparingHostRetirement(cfg Config, repoDir, branch, reviewed string, it Item) (retirementFact, error) {
	next := retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: it.ID, Transport: "host",
		Strategy: cfg.Flows.Verifier.Merge, Title: it.Title, State: "preparing",
		BuiltComment: true,
	}
	if fact, found, err := readRetirement(repoDir, branch, reviewed); err != nil {
		return retirementFact{}, err
	} else if found {
		if fact.Record.ItemID != it.ID {
			return retirementFact{}, fmt.Errorf("%w: retirement %s does not match item %q",
				errHostMergeUnavailable, fact.Ref, it.ID)
		}
		return fact, nil
	}
	facts, err := scanRetirements(repoDir)
	if err != nil {
		return retirementFact{}, err
	}
	for _, fact := range facts {
		if fact.ReadErr != nil {
			badBranch, badID, ok := retirementRefIdentity(fact.Ref)
			if !ok || badBranch == branch || badID == it.ID {
				return retirementFact{}, fact.ReadErr
			}
			continue
		}
		if fact.Record.Branch != branch && fact.Record.ItemID != it.ID {
			continue
		}
		if fact.Record.Branch != branch || fact.Record.ItemID != it.ID ||
			fact.Record.State != "preparing" {
			return retirementFact{}, fmt.Errorf("%w: retirement %s already owns branch %q or item %q",
				errHostMergeUnavailable, fact.Ref, branch, it.ID)
		}
		head, err := branchHead(repoDir, branch)
		if err != nil || head != reviewed {
			return retirementFact{}, fmt.Errorf("%w: %w: retirement %s cannot move from %s to %s without the exact branch head",
				errHostMergeUnavailable, errHostRevisionMoved, fact.Ref, fact.Record.Revision, reviewed)
		}
		merged, err := mergedProjectionPRs(cfg, branch)
		if err != nil {
			return retirementFact{}, retryableHostError(cfg,
				fmt.Errorf("inspect prior Host Revision: %w", err))
		}
		for _, pr := range merged {
			if pr.HeadRefOID != fact.Record.Revision {
				continue
			}
			observed, err := observeHostRetirement(repoDir, fact)
			if err != nil {
				return retirementFact{}, err
			}
			return observed, fmt.Errorf("%w: Host already merged prior Revision %s",
				errHostMergePending, fact.Record.Revision)
		}
		prs, err := openProjectionPR(cfg, branch)
		if err != nil {
			return retirementFact{}, retryableHostError(cfg,
				fmt.Errorf("inspect advanced Host Projection: %w", err))
		}
		if len(prs) == 0 {
			return moveRetirement(repoDir, fact, next)
		}
		if prs[0].HeadRefOID == "" || prs[0].HeadRefOID != reviewed {
			return retirementFact{}, fmt.Errorf("%w: %w: Host Projection for %s reports Revision %s, want %s",
				errHostMergeUnavailable, errHostRevisionMoved, branch, prs[0].HeadRefOID, reviewed)
		}
		return moveRetirement(repoDir, fact, next)
	}
	return recordRetirement(repoDir, next)
}

func pendingHostRetirement(repoDir string, fact retirementFact, verdict verdictNote) (retirementFact, error) {
	if fact.Record.State == "landed" || fact.Record.State == "observed" {
		return fact, nil
	}
	if fact.Record.State != "preparing" && fact.Record.State != "pending" {
		return retirementFact{}, fmt.Errorf("retirement %s cannot become pending from %q", fact.Ref, fact.Record.State)
	}
	pending := fact.Record
	pending.State = "pending"
	pending.Agent = verdict.Reviewer
	pending.Model = verdict.Model
	pending.DefSHA = verdict.DefSHA
	if pending == fact.Record {
		return fact, nil
	}
	return replaceRetirement(repoDir, fact, pending)
}

func resetHostRetirement(repoDir string, fact retirementFact, cause error) (retirementFact, error) {
	preparing := fact.Record
	preparing.State = "preparing"
	preparing.Agent = ""
	preparing.Model = ""
	preparing.DefSHA = ""
	next, err := replaceRetirement(repoDir, fact, preparing)
	if err != nil {
		return fact, err
	}
	return next, fmt.Errorf("%w: %v", errRetirementPreparation, cause)
}

func moveAdvancedHostRetirement(cfg Config, repoDir string, fact retirementFact) (retirementFact, bool, error) {
	head, err := branchHead(repoDir, fact.Record.Branch)
	if err != nil {
		return fact, false, fmt.Errorf("%w: inspect advanced retirement branch: %v",
			errFlowRetryable, err)
	}
	if head == fact.Record.Revision {
		return fact, false, nil
	}
	prs, err := openProjectionPR(cfg, fact.Record.Branch)
	if err != nil {
		return fact, false, retryableHostError(cfg,
			fmt.Errorf("inspect advanced Host Projection: %w", err))
	}
	if len(prs) != 1 || prs[0].HeadRefOID != head {
		return fact, false, nil
	}
	next := fact.Record
	next.Revision = head
	next.State = "preparing"
	next.Agent = ""
	next.Model = ""
	next.DefSHA = ""
	moved, err := moveRetirement(repoDir, fact, next)
	return moved, err == nil, err
}

func observeHostRetirement(repoDir string, fact retirementFact) (retirementFact, error) {
	switch fact.Record.State {
	case "observed", "landed":
		return fact, nil
	case "preparing", "pending":
	default:
		return retirementFact{}, fmt.Errorf("retirement %s cannot become observed from %q",
			fact.Ref, fact.Record.State)
	}
	observed := fact.Record
	observed.State = "observed"
	observed.Agent = ""
	observed.Model = ""
	observed.DefSHA = ""
	return replaceRetirement(repoDir, fact, observed)
}

// recordObservedHostRetirement preserves an exact Host merge before missing
// Builder-comment or approval evidence is recovered. The fact blocks duplicate
// Builder work until recovery joins every required Effect.
func recordObservedHostRetirement(cfg Config, repoDir, branch, reviewed string, it Item) (retirementFact, error) {
	if fact, found, err := readRetirement(repoDir, branch, reviewed); err != nil {
		return retirementFact{}, err
	} else if found {
		if fact.Record.ItemID != it.ID {
			return retirementFact{}, fmt.Errorf("retirement %s does not match item %q", fact.Ref, it.ID)
		}
		return observeHostRetirement(repoDir, fact)
	}
	record := retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: it.ID, Transport: "host",
		Strategy: cfg.Flows.Verifier.Merge, Title: it.Title, State: "observed",
	}
	fact, err := recordRetirementExact(repoDir, record)
	if err == nil {
		return fact, nil
	}
	// Another checkout can publish the approval preparation between the read
	// and compare-and-set. Its exact fact owns recovery.
	if winner, found, readErr := readRetirement(repoDir, branch, reviewed); readErr == nil && found && winner.Record.ItemID == it.ID {
		return observeHostRetirement(repoDir, winner)
	}
	return retirementFact{}, err
}

func recoverRetirementBuiltComment(
	cfg Config,
	repoDir string,
	fact retirementFact,
	it Item,
) (retirementFact, error) {
	if fact.Record.BuiltComment {
		return fact, nil
	}
	if err := publishBuiltComment(
		cfg, repoDir, it, fact.Record.Branch, fact.Record.Revision,
	); err != nil {
		return retirementFact{}, err
	}
	next := fact.Record
	next.BuiltComment = true
	recovered, err := replaceRetirement(repoDir, fact, next)
	if err != nil {
		return retirementFact{}, fmt.Errorf("record completion: %w", err)
	}
	return recovered, nil
}

func landObservedRetirement(repoDir string, fact retirementFact, verdict verdictNote) (retirementFact, error) {
	if fact.Record.State != "observed" {
		return retirementFact{}, fmt.Errorf("retirement %s is not an observed Host merge", fact.Ref)
	}
	landed := fact.Record
	landed.State = "landed"
	landed.Agent = verdict.Reviewer
	landed.Model = verdict.Model
	landed.DefSHA = verdict.DefSHA
	return replaceRetirement(repoDir, fact, landed)
}

func recordHostRetirement(
	cfg Config,
	repoDir, branch, reviewed string,
	it Item,
	verdict verdictNote,
	checks checksNote,
) error {
	if !cfg.Projection.MergeViaHost || verdict.Verdict != "approve" {
		return nil
	}
	fact, err := recordPreparingHostRetirement(cfg, repoDir, branch, reviewed, it)
	if err != nil {
		return err
	}
	if _, _, err := projectBranch(cfg, repoDir, it, branch,
		fmt.Sprintf("Recovered Projection for item #%s: %s.\n", it.ID, it.Title), reviewed); err != nil {
		return err
	}
	if err := projectVerdict(cfg, repoDir, branch, reviewed, verdict, checks); err != nil {
		return err
	}
	_, err = pendingHostRetirement(repoDir, fact, verdict)
	return err
}

func recoverHostMergedProjection(cfg Config, repoDir, branch, reviewed string, it Item, out Outcome) (Outcome, error) {
	fact, err := recordObservedHostRetirement(cfg, repoDir, branch, reviewed, it)
	if err != nil {
		out.Status = "merge_failed"
		return out, err
	}
	fact, err = recoverRetirementBuiltComment(cfg, repoDir, fact, it)
	if err != nil {
		out.Status = "comment_failed"
		return out, fmt.Errorf("comment: %w", err)
	}
	record, err := recoverRetirement(cfg, repoDir, fact, it)
	if err != nil {
		if errors.Is(err, errHostMergePending) {
			out.Status = "merge_pending"
			return out, nil
		}
		out.Status = "merge_failed"
		return out, err
	}
	out.Agent = record.Agent
	out.Model = record.Model
	out.DefSHA = record.DefSHA
	out.Verdict = "approve"
	out.Status = "merged"
	return out, nil
}

func retirementEvidenceReadError(kind string, err error) error {
	if errors.Is(err, errNoteInvalid) {
		return fmt.Errorf("%w: read durable %s: %v", errRetirementEvidenceInvalid, kind, err)
	}
	return fmt.Errorf("%w: read durable %s: %v", errHostMergePending, kind, err)
}

func readRetirementApproval(repoDir, revision string) (verdictNote, checksNote, error) {
	if err := fetchNotes(repoDir); err != nil {
		return verdictNote{}, checksNote{}, fmt.Errorf("%w: refresh durable notes: %v", errHostMergePending, err)
	}
	verdict, _, err := readVerdict(repoDir, revision)
	if err != nil {
		return verdictNote{}, checksNote{}, retirementEvidenceReadError("Verdict", err)
	}
	checks, _, err := readChecks(repoDir, revision)
	if err != nil {
		return verdictNote{}, checksNote{}, retirementEvidenceReadError("Checks", err)
	}
	return verdict, checks, nil
}

func recordHostMergeHandoff(
	cfg Config,
	repoDir string,
	record retirementRecord,
	it Item,
	handoff error,
) error {
	if err := recordMergeBlocked(
		cfg, repoDir, retirementSubjectKey(record.Branch), record.Revision, it, handoff,
	); err != nil {
		if errors.Is(err, errFlowRetryable) || errors.Is(err, errTrackerUnavailable) {
			return fmt.Errorf("%w: %v; publish Host merge handoff: %v",
				errHostMergePending, handoff, err)
		}
		return fmt.Errorf("%w: %v; publish Host merge handoff: %v",
			errRetirementRecoveryHard, handoff, err)
	}
	return fmt.Errorf("%w: %v", errRetirementRecoveryHard, handoff)
}
