package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func landRetirement(repoDir string, fact retirementFact) (retirementFact, error) {
	if fact.Record.State == "landed" {
		return fact, nil
	}
	landed := fact.Record
	landed.State = "landed"
	return replaceRetirement(repoDir, fact, landed)
}

func (f verifierFlow) actRetirement(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	fact, found, err := readRetirement(repoDir, s.Branch, s.Revision)
	if err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: s.Revision, Status: "merge_failed"}, err
	}
	if !found {
		return Outcome{Branch: s.Branch, BaseSHA: s.Revision, Status: "skipped"}, nil
	}
	record := fact.Record
	out := Outcome{
		Branch: s.Branch, BaseSHA: s.Revision,
		Agent: record.Agent, Model: record.Model, DefSHA: record.DefSHA,
	}
	fact, err = recoverRetirementBuiltComment(cfg, repoDir, fact, s.Item)
	if err != nil {
		out.Status = "comment_failed"
		return out, fmt.Errorf("comment: %w", err)
	}
	record = fact.Record
	if record.State == "preparing" {
		head, present, headErr := lookupBranchHead(repoDir, record.Branch)
		if headErr != nil {
			out.Status = "merge_pending"
			return out, nil
		}
		if present {
			if head != record.Revision {
				moved, moveErr := recordPreparingHostRetirement(
					cfg, repoDir, record.Branch, head, s.Item)
				out.BaseSHA = head
				if moveErr != nil {
					if errors.Is(moveErr, errHostMergePending) ||
						errors.Is(moveErr, errRetirementPreparation) {
						out.Status = "merge_pending"
						return out, nil
					}
					out.Status = "merge_failed"
					return out, moveErr
				}
				if moved.Record.Revision != head {
					out.Status = "merge_failed"
					return out, fmt.Errorf("%w: preparing retirement did not move to the live Revision",
						errHostMergeUnavailable)
				}
				out.Status = "skipped"
				return out, nil
			}
			verdict, checks, evidenceErr := readRetirementApproval(repoDir, record.Revision)
			if evidenceErr != nil {
				if errors.Is(evidenceErr, errHostMergePending) {
					out.Status = "merge_pending"
					return out, nil
				}
				out.Status = "merge_failed"
				return out, evidenceErr
			}
			terminal := checks.Status == "fail" || verdict.Verdict == "changes"
			if terminal {
				merged, _, inspectErr := inspectProjectMerge(
					cfg, record.Branch, record.Strategy, record.Revision)
				if inspectErr != nil {
					inspectErr = retryableHostError(cfg, inspectErr)
					if errors.Is(inspectErr, errHostMergePending) {
						out.Status = "merge_pending"
						return out, nil
					}
					out.Status = "merge_failed"
					return out, inspectErr
				}
				if !merged {
					out.Status = "reviewed"
					if checks.Status == "fail" {
						out.Status = "checks_failed"
					}
					out.Verdict = verdict.Verdict
					return out, nil
				}
			} else {
				agentStalled, stallErr := stalledOn(repoDir, f.Name(),
					retirementAgentSubjectKey(record.Branch), record.Revision)
				if stallErr != nil {
					out.Status = "merge_failed"
					return out, stallErr
				}
				branchStalled, stallErr := stalledOn(repoDir, f.Name(),
					"branch-"+record.Branch, record.Revision)
				if stallErr != nil {
					out.Status = "merge_failed"
					return out, stallErr
				}
				if !agentStalled && !branchStalled {
					branchSubject := s
					branchSubject.Key = retirementAgentSubjectKey(record.Branch)
					branchSubject.Kind = subjectBranch
					branchSubject.Revision = head
					return f.actBranch(cfg, repoDir, branchSubject, runID)
				}
			}
		}
	}
	recovered, err := recoverRetirement(cfg, repoDir, fact, s.Item)
	if recovered.Agent != "" {
		out.Agent = recovered.Agent
		out.Model = recovered.Model
		out.DefSHA = recovered.DefSHA
	}
	if err != nil {
		switch {
		case errors.Is(err, errHostMergePending):
			out.Status = "merge_pending"
			return out, nil
		case errors.Is(err, errRetirementPreparation):
			out.Status = "skipped"
			return out, nil
		case errors.Is(err, errRetirementStale):
			out.Status = "stale"
			return out, err
		default:
			out.Status = "merge_failed"
			return out, err
		}
	}
	out.Status = "merged"
	return out, nil
}

// mergeVerified lands only the Revision that carried the approving Verdict.
// A retirement fact makes the multi-system effect resumable. Git writes its
// landed fact atomically with master. The host path writes pending intent first,
// then promotes it after the host reports the exact reviewed head as merged.
func mergeVerified(cfg Config, repoDir, branch, reviewed string, it Item, a *Agent) error {
	if fact, found, err := readRetirement(repoDir, branch, reviewed); err != nil {
		return err
	} else if found {
		return recoverRetirementFact(cfg, repoDir, fact, it)
	}
	if cfg.Projection.MergeViaHost {
		// The Host request's exact head is the revision fence. This path also
		// recovers after the Host merged and deleted the source branch.
		return mergeHostPath(cfg, repoDir, branch, reviewed, it, a)
	}
	if err := fenceMergeOnRevision(repoDir, branch, reviewed); err != nil {
		return err
	}
	return mergeGitPath(cfg, repoDir, branch, reviewed, it, a)
}

func mergeHostPath(cfg Config, repoDir, branch, reviewed string, it Item, _ *Agent) error {
	verdict, checks, err := readRetirementApproval(repoDir, reviewed)
	if err != nil {
		return err
	}
	if verdict.Verdict != "approve" || checks.Status != "pass" {
		return fmt.Errorf("%w: Host merge requires durable approval", errRetirementEvidenceInvalid)
	}
	if err := recordHostRetirement(cfg, repoDir, branch, reviewed, it, verdict, checks); err != nil {
		return err
	}
	fact, found, err := readRetirement(repoDir, branch, reviewed)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: Host retirement disappeared after publication", errFlowRetryable)
	}
	return recoverRetirementFact(cfg, repoDir, fact, it)
}

// mergeGitPath commits the retirement fact in the same atomic push that
// advances master. A retry therefore skips merge construction and resumes at
// Tracker retirement, including squash merges whose tree is already on master.
func mergeGitPath(cfg Config, repoDir, branch, reviewed string, it Item, a *Agent) error {
	if fact, found, err := readRetirement(repoDir, branch, reviewed); err != nil {
		return err
	} else if found {
		return recoverRetirementFact(cfg, repoDir, fact, it)
	}
	workspace := workspaceDir(repoDir)
	mergeDir := filepath.Join(workspace, "worktrees", "merge-"+slug(branch))
	if err := trackWorktree(mergeDir); err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	defer cleanupWorktree(repoDir, mergeDir)
	_ = os.RemoveAll(mergeDir)
	if err := git(repoDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("merge: prune: %w", err)
	}
	if err := git(repoDir, "fetch", "origin", "master"); err != nil {
		return fmt.Errorf("merge: fetch master: %w", err)
	}
	masterTip, err := gitOut(repoDir, "rev-parse", "origin/master")
	if err != nil {
		return fmt.Errorf("merge: origin/master: %w", err)
	}
	if err := git(repoDir, "worktree", "add", "--detach", mergeDir, "origin/master"); err != nil {
		return fmt.Errorf("merge: worktree: %w", err)
	}
	switch cfg.Flows.Verifier.Merge {
	case "squash":
		if err := git(mergeDir, "merge", "--squash", reviewed); err != nil {
			return fmt.Errorf("merge: squash: %w", err)
		}
		if err := gitCommit(mergeDir, a.Commit, fmt.Sprintf("forest: %s (#%s)", it.Title, it.ID)); err != nil {
			return fmt.Errorf("merge: commit: %w", err)
		}
	case "ff":
		if err := git(mergeDir, "merge", "--ff-only", reviewed); err != nil {
			return fmt.Errorf("merge: ff: %w", err)
		}
	default:
		return fmt.Errorf("merge: unsupported strategy %q", cfg.Flows.Verifier.Merge)
	}
	fact, err := prepareRetirement(repoDir, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: it.ID, Transport: "git",
		Strategy: cfg.Flows.Verifier.Merge, Title: it.Title, State: "landed",
		Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA, BuiltComment: true,
	})
	if err != nil {
		return err
	}
	if pushErr := git(mergeDir, "push", "--atomic",
		"--force-with-lease=refs/heads/master:"+masterTip,
		"--force-with-lease=refs/heads/"+branch+":"+reviewed,
		"--force-with-lease="+fact.Ref+":",
		"origin", "HEAD:master", reviewed+":refs/heads/"+branch, fact.SHA+":"+fact.Ref); pushErr != nil {
		current, found, readErr := readRetirement(repoDir, branch, reviewed)
		if readErr != nil {
			return readErr
		}
		if found {
			return recoverRetirementFact(cfg, repoDir, current, it)
		}
		head, present, headErr := lookupBranchHead(repoDir, branch)
		if headErr != nil {
			return fmt.Errorf("%w: reconcile merge push: %v", errFlowRetryable, headErr)
		}
		if !present || head != reviewed {
			return fmt.Errorf("%w: merge source branch moved after failed push", errRetirementStale)
		}
		return fmt.Errorf("%w: merge push: %v", errFlowRetryable, pushErr)
	}
	return finishRetirement(cfg, repoDir, fact, it)
}

func recoverRetirementFact(cfg Config, repoDir string, fact retirementFact, it Item) error {
	_, err := recoverRetirement(cfg, repoDir, fact, it)
	return err
}

func recoverRetirement(cfg Config, repoDir string, fact retirementFact, it Item) (retirementRecord, error) {
	record := fact.Record
	if record.ItemID != it.ID || record.Branch == "" || record.Revision == "" {
		return record, fmt.Errorf("retirement %s does not match item %q", fact.Ref, it.ID)
	}
	if record.State == "preparing" || record.State == "pending" {
		if record.Transport != "host" {
			return record, fmt.Errorf("retirement %s is active without Host transport", fact.Ref)
		}
		starting := record.State
		hostMerged, _, hostErr := inspectProjectMerge(cfg, record.Branch, record.Strategy, record.Revision)
		if hostErr != nil {
			if errors.Is(hostErr, errHostRevisionMoved) {
				moved, changed, moveErr := moveAdvancedHostRetirement(cfg, repoDir, fact)
				if moveErr != nil {
					return record, moveErr
				}
				if changed {
					return moved.Record, fmt.Errorf("%w: Host Projection advanced to Revision %s",
						errRetirementPreparation, moved.Record.Revision)
				}
				return record, fmt.Errorf("%w: moved Host Projection has no exact migration target",
					errHostMergeUnavailable)
			}
			if errors.Is(hostErr, errHostMergeNoView) {
				if starting == "preparing" {
					head, found, headErr := lookupBranchHead(repoDir, record.Branch)
					if headErr != nil {
						return record, fmt.Errorf("%w: inspect preparing branch: %v", errHostMergePending, headErr)
					}
					if !found {
						return record, fmt.Errorf("%w: preparing branch %q has no Host view or remote Revision",
							errRetirementRecoveryHard, record.Branch)
					}
					if head != record.Revision {
						moved, moveErr := recordPreparingHostRetirement(cfg, repoDir, record.Branch, head, it)
						if moveErr != nil {
							return record, moveErr
						}
						return moved.Record, fmt.Errorf("%w: Host branch advanced to Revision %s before Projection",
							errRetirementPreparation, moved.Record.Revision)
					}
					_, merged, projectErr := projectBranch(cfg, repoDir, it, record.Branch,
						fmt.Sprintf("Recovered Projection for item #%s: %s.\n", it.ID, it.Title), record.Revision)
					if projectErr != nil {
						return record, retryableHostError(cfg, projectErr)
					}
					if merged {
						observed, observeErr := observeHostRetirement(repoDir, fact)
						if observeErr != nil {
							return record, observeErr
						}
						return observed.Record, errHostMergePending
					}
					return record, errRetirementPreparation
				}
				return record, fmt.Errorf("%w: %v", errHostMergePending, hostErr)
			}
			if errors.Is(hostErr, errHostMergeUnavailable) {
				return record, hostErr
			}
			return record, fmt.Errorf("%w: %v", errHostMergePending, hostErr)
		}
		if hostMerged {
			observed, err := observeHostRetirement(repoDir, fact)
			if err != nil {
				return record, err
			}
			fact = observed
			record = fact.Record
		}
		verdict, checks, err := readRetirementApproval(repoDir, record.Revision)
		if err != nil {
			return record, fmt.Errorf("retirement %s: %w", fact.Ref, err)
		}
		approved := verdict.Verdict == "approve" && checks.Status == "pass"
		matches := starting == "preparing" || record.Agent == verdict.Reviewer &&
			record.Model == verdict.Model && record.DefSHA == verdict.DefSHA
		if hostMerged {
			if !approved {
				return record, errHostMergePending
			}
			fact, err = landObservedRetirement(repoDir, fact, verdict)
			if err != nil {
				return record, err
			}
			record = fact.Record
		} else {
			if !approved {
				if starting == "pending" {
					next, resetErr := resetHostRetirement(repoDir, fact,
						fmt.Errorf("retirement %s lacks a durable approve Verdict and passing Checks", fact.Ref))
					return next.Record, resetErr
				}
				return record, errHostMergePending
			}
			if starting == "preparing" || !matches {
				fact, err = pendingHostRetirement(repoDir, fact, verdict)
				if err != nil {
					return record, err
				}
				record = fact.Record
			}
			if !cfg.Flows.Verifier.AutoMerge {
				return record, errHostMergePending
			}
			attemptKey := effectAttemptKey("Host-merge-request", record.Branch, record.Revision)
			acceptedKey := effectAttemptKey("Host-merge-accepted", record.Branch, record.Revision)
			attempts, attemptsErr := readAttempts(repoDir, attemptKey)
			if attemptsErr != nil {
				return record, attemptsErr
			}
			accepted, acceptedErr := readAttempts(repoDir, acceptedKey)
			if acceptedErr != nil {
				return record, acceptedErr
			}
			if attempts != 0 {
				if attempts == 1 && accepted == 1 {
					return record, errHostMergePending
				}
				handoff := fmt.Errorf(
					"Host merge request for Revision %s has an uncertain durable request outcome",
					record.Revision,
				)
				return record, recordHostMergeHandoff(cfg, repoDir, record, it, handoff)
			}
			if accepted != 0 {
				return record, fmt.Errorf("%w: Host merge acceptance exists without its request claim",
					errAttemptsInvalid)
			}
			attempts, attemptsErr = bumpAttempts(repoDir, attemptKey)
			if attemptsErr != nil {
				return record, attemptsErr
			}
			if attempts != 1 {
				handoff := fmt.Errorf(
					"Host merge request for Revision %s has concurrent durable request claims",
					record.Revision,
				)
				return record, recordHostMergeHandoff(cfg, repoDir, record, it, handoff)
			}
			if err := projectMerge(cfg, record.Branch, record.Strategy, record.Revision); err != nil {
				if errors.Is(err, errHostMergeAccepted) {
					accepted, acceptedErr = bumpAttempts(repoDir, acceptedKey)
					if acceptedErr != nil {
						return record, fmt.Errorf("%w: persist accepted Host merge request: %v",
							errHostMergePending, acceptedErr)
					}
					if accepted != 1 {
						handoff := fmt.Errorf(
							"Host merge request for Revision %s has concurrent acceptance claims",
							record.Revision,
						)
						return record, recordHostMergeHandoff(cfg, repoDir, record, it, handoff)
					}
					if errors.Is(err, errHostMergeUnavailable) {
						handoff := fmt.Errorf(
							"accepted Host merge request for Revision %s lost its exact Host identity",
							record.Revision,
						)
						return record, recordHostMergeHandoff(cfg, repoDir, record, it, handoff)
					}
					return record, errHostMergePending
				}
				if errors.Is(err, errHostMergeUnavailable) {
					return record, err
				}
				if errors.Is(err, errHostMergeRequestFailed) {
					handoff := fmt.Errorf(
						"Host merge request for Revision %s failed during its single durable request",
						record.Revision,
					)
					return record, recordHostMergeHandoff(cfg, repoDir, record, it, handoff)
				}
				return record, fmt.Errorf("%w: %v", errHostMergePending, err)
			}
			fact, err = landRetirement(repoDir, fact)
			if err != nil {
				return record, err
			}
			record = fact.Record
		}
	}
	if record.State == "observed" {
		verdict, checks, err := readRetirementApproval(repoDir, record.Revision)
		if err != nil {
			return record, fmt.Errorf("retirement %s: %w", fact.Ref, err)
		}
		approved := verdict.Verdict == "approve" && checks.Status == "pass"
		if !approved {
			return record, errHostMergePending
		}
		fact, err = landObservedRetirement(repoDir, fact, verdict)
		if err != nil {
			return record, err
		}
		record = fact.Record
	}
	return record, finishRetirement(cfg, repoDir, fact, it)
}
func finishRetirement(cfg Config, repoDir string, fact retirementFact, it Item) error {
	record := fact.Record
	// The durable retirement fact replaces the source branch as recovery
	// evidence. Delete the exact reviewed branch first, so an advanced branch is
	// refused before the Tracker item closes and a failed Close stays resumable.
	if err := deleteReviewedBranch(repoDir, record.Branch, record.Revision); err != nil {
		if errors.Is(err, errRetirementStale) {
			return err
		}
		return fmt.Errorf("%w: %v", errFlowRetryable, err)
	}
	// A request claim exists before the Tracker close. An acceptance claim
	// records the successful return before later cleanup can retry.
	closeKey := effectAttemptKey("Tracker-close", it.ID, record.Revision)
	acceptedCloseKey := effectAttemptKey("Tracker-close-accepted", it.ID, record.Revision)
	closeClaim, err := readAttempts(repoDir, closeKey)
	if err != nil {
		return err
	}
	acceptedClose, err := readAttempts(repoDir, acceptedCloseKey)
	if err != nil {
		return err
	}
	if acceptedClose != 0 {
		if closeClaim != 1 || acceptedClose != 1 {
			return fmt.Errorf("%w: Tracker close acceptance has no single request claim",
				errAttemptsInvalid)
		}
	} else {
		if closeClaim != 0 {
			return fmt.Errorf("%w: Tracker close for Item %q has an uncertain outcome",
				errHostMergeUnavailable, it.ID)
		}
		if err := claimEffect(repoDir, "Tracker-close", it.ID, record.Revision); err != nil {
			return err
		}
		if closeErr := trackerFor(cfg.Repo).Close(it.ID); closeErr != nil {
			if errors.Is(closeErr, errTrackerEffectNotApplied) {
				if err := dropAttempts(repoDir, closeKey); err != nil {
					return fmt.Errorf("%w: reset unapplied Tracker close: %v",
						errHostMergeUnavailable, err)
				}
				return fmt.Errorf("merge: close item: %w", closeErr)
			}
			if errors.Is(closeErr, errTrackerEvidenceInvalid) {
				return closeErr
			}
			return fmt.Errorf("%w: Tracker close outcome for Item %q is uncertain: %v",
				errHostMergeUnavailable, it.ID, closeErr)
		}
		if err := claimEffect(repoDir, "Tracker-close-accepted", it.ID, record.Revision); err != nil {
			return fmt.Errorf("%w: persist accepted Tracker close: %v",
				errHostMergeUnavailable, err)
		}
	}
	branchEffects, err := listEffectRefs(repoDir, record.Branch)
	if err != nil {
		return err
	}
	itemEffects, err := listEffectRefs(repoDir, it.ID)
	if err != nil {
		return err
	}
	optionalRefs := []string{
		"refs/forest/attempt/branch-" + record.Branch,
		stalledRef("builder", "item-"+it.ID),
		stalledRef("fixer", "branch-"+record.Branch),
		stalledRef("verifier", "branch-"+record.Branch),
		stalledRef("verifier", retirementSubjectKey(record.Branch)),
		stalledRef("verifier", retirementAgentSubjectKey(record.Branch)),
	}
	optionalRefs = append(optionalRefs, branchEffects...)
	optionalRefs = append(optionalRefs, itemEffects...)
	if err := deleteRefsAtomically(repoDir, fact.Ref, fact.SHA, optionalRefs...); err != nil {
		return fmt.Errorf("%w: merge: retire durable refs: %v", errFlowRetryable, err)
	}
	return nil
}

func deleteReviewedBranch(repoDir, branch, reviewed string) error {
	out, err := gitCommand(repoDir, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("merge: inspect branch %s: %w", branch, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return nil
	}
	if fields[0] != reviewed {
		return fmt.Errorf("%w: merge: branch %s advanced to %s before retirement of %s", errRetirementStale, branch, fields[0], reviewed)
	}
	if err := deleteRef(repoDir, "refs/heads/"+branch, reviewed); err != nil {
		return fmt.Errorf("merge: delete branch %s (wanted %s): %w", branch, reviewed, err)
	}
	return nil
}
