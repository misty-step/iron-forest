package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type verifierFlow struct{}

func (verifierFlow) Name() string { return "verifier" }

func (verifierFlow) Interval(cfg Config) time.Duration {
	return time.Duration(cfg.Flows.Verifier.IntervalSec) * time.Second
}

func (verifierFlow) Enabled(cfg Config) bool { return cfg.Flows.Verifier.Enabled }

func retryableHostError(cfg Config, err error) error {
	if !cfg.Projection.Enabled || errors.Is(err, errHostMergePending) ||
		errors.Is(err, errFlowRetryable) ||
		errors.Is(err, errRetirementEvidenceInvalid) ||
		errors.Is(err, errAttemptsInvalid) ||
		errors.Is(err, errControlEvidenceInvalid) ||
		errors.Is(err, errHostMergeUnavailable) && !errors.Is(err, errHostRevisionMoved) ||
		!cfg.Projection.MergeViaHost && errors.Is(err, errHostRevisionMoved) {
		return err
	}
	return fmt.Errorf("%w: %w", errHostMergePending, err)
}

func retirementProjectionError(cfg Config, err error) error {
	err = retryableHostError(cfg, err)
	if cfg.Projection.MergeViaHost &&
		errors.Is(err, errHostMergeUnavailable) &&
		!errors.Is(err, errHostRevisionMoved) {
		return fmt.Errorf("%w: %w", errRetirementRecoveryHard, err)
	}
	return err
}

func flowNoteError(err error) error {
	if errors.Is(err, errNoteInvalid) {
		return fmt.Errorf("%w: %v", errRetirementEvidenceInvalid, err)
	}
	return fmt.Errorf("%w: %v", errFlowRetryable, err)
}

func retirementSubjectKey(branch string) string {
	return "retirement-" + branch
}

func retirementAgentSubjectKey(branch string) string {
	return "retirement-agent-" + branch
}

func (verifierFlow) Select(cfg Config, repoDir string) ([]Subject, error) {
	updateGate.RLock()
	defer updateGate.RUnlock()
	if err := fetchNotes(repoDir); err != nil {
		return nil, fmt.Errorf("notes: %w", err)
	}
	branches, err := forestBranches(repoDir)
	if err != nil {
		return nil, err
	}
	branchHeads := make(map[string]string, len(branches))
	for _, branch := range branches {
		head, err := branchHead(repoDir, branch)
		if err != nil {
			return nil, fmt.Errorf("branch %s: %w", branch, err)
		}
		branchHeads[branch] = head
	}
	retirements, err := scanRetirements(repoDir)
	if err != nil {
		return nil, fmt.Errorf("retirements: %w", err)
	}
	var recoveries []Subject
	retiring := make(map[string]bool, len(retirements))
	for _, fact := range retirements {
		var s Subject
		if fact.ReadErr != nil {
			s = Subject{
				Key:      "retirement-evidence-" + blobSHA(fact.Ref),
				Kind:     subjectRetirement,
				Revision: fact.SHA,
				Label:    "invalid retirement evidence",
				Failure:  fact.ReadErr,
			}
			if branch, id, ok := retirementRefIdentity(fact.Ref); ok {
				s.ID = id
				s.Branch = branch
				s.Item = Item{ID: id}
				retiring[branch] = true
			}
		} else {
			record := fact.Record
			retiring[record.Branch] = true
			s = Subject{Key: retirementSubjectKey(record.Branch), Kind: subjectRetirement,
				Revision: record.Revision, Label: "retire " + record.Branch,
				ID: record.ItemID, Branch: record.Branch, Item: Item{ID: record.ItemID, Title: record.Title}}
		}
		stalled, err := stalledOn(repoDir, "verifier", s.Key, s.Revision)
		if err != nil {
			return nil, fmt.Errorf("stalled %s: %w", s.Key, err)
		}
		if !stalled {
			recoveries = append(recoveries, s)
		}
	}
	var fresh, mergeable []Subject
	for _, branch := range branches {
		head := branchHeads[branch]
		if retiring[branch] {
			continue
		}
		s := Subject{Key: "branch-" + branch,
			Kind:     subjectBranch,
			Revision: head,
			Label:    branch,
			ID:       itemIDFromBranch(branch),
			Branch:   branch}
		stalled, err := stalledOn(repoDir, "verifier", s.Key, head)
		if err != nil {
			return nil, fmt.Errorf("stalled %s: %w", s.Key, err)
		}
		if stalled {
			continue
		}
		v, found, err := readVerdict(repoDir, head)
		if err != nil {
			if errors.Is(err, errNoteInvalid) {
				s.Failure = flowNoteError(err)
				fresh = append(fresh, s)
				continue
			}
			return nil, fmt.Errorf("verdict %s: %w", branch, err)
		}
		if !found {
			// A head whose checks already failed belongs to the Fixer: rechecking
			// the same commit buys the same fact and pays for it again. A repaired
			// branch has a new head carrying no notes, so it returns here.
			checks, hasChecks, cerr := readChecks(repoDir, head)
			if cerr != nil {
				if errors.Is(cerr, errNoteInvalid) {
					s.Failure = flowNoteError(cerr)
					fresh = append(fresh, s)
					continue
				}
				return nil, fmt.Errorf("checks %s: %w", branch, cerr)
			}
			if hasChecks && checks.Status == "fail" {
				continue
			}
			fresh = append(fresh, s)
			continue
		}
		if v.Verdict != "approve" {
			continue
		}
		checks, found, err := readChecks(repoDir, head)
		if err != nil {
			if errors.Is(err, errNoteInvalid) {
				s.Failure = flowNoteError(err)
				fresh = append(fresh, s)
				continue
			}
			return nil, fmt.Errorf("checks %s: %w", branch, err)
		}
		if !found {
			fresh = append(fresh, s)
			continue
		}
		if checks.Status != "pass" {
			continue
		}
		// An approved, green branch is a subject when it can land. With Host
		// projection and AutoMerge disabled, select it once to prepare durable
		// intent; the resulting retirement fact suppresses later branch work.
		attempts, err := readAttempts(repoDir, s.Key)
		if err != nil {
			if errors.Is(err, errAttemptsInvalid) {
				s.Failure = err
				mergeable = append(mergeable, s)
				continue
			}
			return nil, fmt.Errorf("attempts %s: %w", branch, err)
		}
		if mergeBlocked(cfg, attempts) != "" &&
			!(cfg.Projection.MergeViaHost && !cfg.Flows.Verifier.AutoMerge) {
			continue
		}
		mergeable = append(mergeable, s)
	}
	recoveries = append(recoveries, fresh...)
	recoveries = append(recoveries, mergeable...)
	return recoveries, nil
}

func hasCommentMarker(comments []comment, marker string) bool {
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return true
		}
	}
	return false
}

func publishMergeBlocked(t Tracker, it Item, revision string, cause error) error {
	marker := "<!-- iron-forest:merge-blocked revision=" + revision + " -->"
	if hasCommentMarker(it.Comments, marker) {
		return nil
	}
	current, readErr := validatedTrackerItem(t, it.ID)
	if readErr != nil {
		return fmt.Errorf("reconcile handoff comment: %w", readErr)
	}
	if hasCommentMarker(current.Comments, marker) {
		return nil
	}
	body := "Merge blocked: " + redactSecretShaped(cause.Error()) + "\n\n" + marker
	if err := t.Comment(it.ID, body); err != nil {
		current, readErr := validatedTrackerItem(t, it.ID)
		if readErr != nil {
			return fmt.Errorf("comment: %v; reconcile: %w", err, readErr)
		}
		if hasCommentMarker(current.Comments, marker) {
			return nil
		}
		return err
	}
	return nil
}

// mergeBlocked names why an approved, green branch cannot complete a merge,
// or returns "" when it may. Select uses it to avoid no-op work, except that a
// Host branch with AutoMerge disabled gets one pass to record durable intent.
// Act still consults the same authority after that preparation, so it reports
// the operator handoff without repeating work.
func mergeBlocked(cfg Config, attempts int) string {
	if !cfg.Flows.Verifier.AutoMerge {
		return "auto_merge is off; an operator merges this branch"
	}
	if attempts >= cfg.Flows.Fixer.Attempts {
		return "merge attempts exhausted; a human is required"
	}
	return ""
}

func (f verifierFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	if s.Failure != nil {
		return Outcome{
			Branch: s.Branch, BaseSHA: s.Revision, Status: "evidence_failed",
		}, s.Failure
	}
	if s.Kind == subjectRetirement {
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
	return f.actBranch(cfg, repoDir, s, runID)
}

func (verifierFlow) actBranch(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	out := Outcome{Branch: s.Branch, BaseSHA: s.Revision}
	stalled, err := stalledOn(repoDir, "verifier", s.Key, s.Revision)
	if err != nil {
		out.Status = "notes_failed"
		return out, err
	}
	if stalled {
		out.Status = "skipped"
		return out, nil
	}
	it, err := validatedTrackerItem(trackerFor(cfg.Repo), s.ID)
	if err != nil {
		out.Status = "item_failed"
		return out, fmt.Errorf("item: %w", err)
	}
	if cfg.Projection.MergeViaHost {
		if _, err := recordPreparingHostRetirement(cfg, repoDir, s.Branch, s.Revision, it); err != nil {
			out.Status = "projection_failed"
			return out, fmt.Errorf("projection preparation: %w", retryableHostError(cfg, err))
		}
	}
	workspace := workspaceDir(repoDir)
	wtDir, baseSHA, err := createWorktreeAtBranch(repoDir, workspace, s.Branch)
	if err != nil {
		if cfg.Projection.MergeViaHost && s.Revision != "" {
			merged, pr, inspectErr := inspectProjectMerge(cfg, s.Branch, cfg.Flows.Verifier.Merge, s.Revision)
			if inspectErr != nil {
				out.Status = "projection_failed"
				return out, fmt.Errorf("projection: %w", retirementProjectionError(cfg, inspectErr))
			}
			if merged && pr.HeadRefOID == s.Revision {
				out.PRURL = pr.URL
				return recoverHostMergedProjection(cfg, repoDir, s.Branch, s.Revision, it, out)
			}
		}
		out.Status = "worktree_failed"
		return out, fmt.Errorf("worktree: %w", err)
	}
	defer cleanupWorktree(repoDir, wtDir)
	if err := checkSubjectRevision(s, baseSHA); err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: baseSHA, Status: "stale"}, err
	}
	a, err := loadAgent(repoDir, cfg.Flows.Verifier.Agent)
	if err != nil {
		out.Status = "agent_failed"
		return out, fmt.Errorf("agent: %w", err)
	}
	out = Outcome{Branch: s.Branch, BaseSHA: baseSHA, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA}
	url, hostMerged, err := projectBranch(cfg, repoDir, it, s.Branch,
		fmt.Sprintf("Recovered Projection for item #%s: %s.\n", it.ID, it.Title), baseSHA)
	if err != nil {
		out.Status = "projection_failed"
		return out, fmt.Errorf("projection: %w", retryableHostError(cfg, err))
	}
	out.PRURL = url
	if hostMerged {
		return recoverHostMergedProjection(cfg, repoDir, s.Branch, baseSHA, it, out)
	}
	// Rebase the branch onto current master before checking or reviewing it: a
	// Verdict and its checks must key to the exact tree that will land, not to a
	// tree built from an ancient master. Every later step uses the returned head.
	newHead, rebaseErr := rebaseOntoMaster(wtDir, s.Branch, baseSHA, a.Commit)
	if rebaseErr != nil {
		// A conflict is a fact about this head, so record it where every lane
		// already looks: a failing check on the commit. The Fixer selects failing
		// checks, so a conflict becomes work instead of a dead end, and this lane
		// stops offering the head because the fact outlives the pass. A tracker
		// label cannot do this job: the factory never reads its own labels back.
		out.Status = "checks_failed"
		note := checksNote{
			Status:  "fail",
			RunID:   runID,
			Time:    nowRFC(),
			Results: []checkResult{{Name: "rebase", Code: 1, Output: rebaseErr.Error()}},
		}
		if err := writeChecks(repoDir, baseSHA, note); err != nil {
			if !errors.Is(err, errNoteExists) {
				return out, fmt.Errorf("rebase: %v (notes: %w)", rebaseErr, flowNoteError(err))
			}
			winner, found, readErr := readChecks(repoDir, baseSHA)
			if readErr != nil {
				return out, fmt.Errorf("rebase: %v (notes: read winning check: %w)",
					rebaseErr, flowNoteError(readErr))
			}
			if !found {
				return out, fmt.Errorf("rebase: %w (notes: check note disappeared: %v)", rebaseErr, err)
			}
			note = winner
		}
		if err := projectChecks(cfg, s.Branch, baseSHA, note); err != nil {
			return out, fmt.Errorf("rebase: %v (projection: %w)", rebaseErr, retryableHostError(cfg, err))
		}
		return out, fmt.Errorf("rebase: %w", rebaseErr)
	}
	baseSHA = newHead
	// The ledger must record the head the checks, the Verdict, and the merge all
	// key to; the value above was the pre-rebase head.
	out.BaseSHA = newHead
	if cfg.Projection.MergeViaHost {
		if _, err := recordPreparingHostRetirement(cfg, repoDir, s.Branch, newHead, it); err != nil {
			out.Status = "projection_failed"
			return out, fmt.Errorf("projection preparation after rebase: %w", retryableHostError(cfg, err))
		}
	}

	// Snapshot the pristine reviewed tree before any agent code runs, so a check
	// that rewrites or stages a tracked file is visible afterwards: its green
	// would then judge an uncommitted edit, never the Review revision it was
	// declared to test.
	beforeChecks, err := snapshotReviewTree(wtDir)
	if err != nil {
		out.Status = "checks_environment_failed"
		return out, fmt.Errorf("review snapshot: %w", err)
	}
	checks, checkErr := runChecks(cfg, wtDir, runID)
	// A preflight failure means no declared check ran because the child
	// environment could not be built. There is nothing to record, so no Checks
	// note exists, and the
	// head is not broken code for the Fixer to repair. Write no note, classify it
	// as a mechanical failure for an operator, and let the stalled brake park the
	// head here instead of reviewing or merging a Revision whose checks never ran.
	if checkErr != nil && checks.Status == "" {
		out.Status = "checks_environment_failed"
		return out, fmt.Errorf("checks: %w", checkErr)
	}
	// A check that rewrote or staged a tracked file — or moved HEAD — is refused:
	// the green it reports is an artifact of its own uncommitted edit, not of the
	// Review revision. No Checks note is written, so the head is neither offered
	// to a reviewer (no Verdict may rest on the untrustworthy result) nor to the
	// Fixer (there is nothing to repair in the committed tree).
	if cleanErr := assertChecksClean(wtDir, baseSHA, beforeChecks); cleanErr != nil {
		out.Status = "checks_refused"
		return out, fmt.Errorf("checks: %w", cleanErr)
	}
	if err := writeChecks(repoDir, baseSHA, checks); err != nil {
		if !errors.Is(err, errNoteExists) {
			out.Status = "notes_failed"
			return out, fmt.Errorf("notes: %w", flowNoteError(err))
		}
		winner, found, readErr := readChecks(repoDir, baseSHA)
		if readErr != nil {
			out.Status = "notes_failed"
			return out, fmt.Errorf("notes: read winning check: %w", flowNoteError(readErr))
		}
		if !found {
			out.Status = "notes_failed"
			return out, fmt.Errorf("notes: check note disappeared: %w", err)
		}
		// The winner note is the write-once fact for this Revision, but its mere
		// existence never clears a real check error below: the note's own status
		// decides routing, and a preflight failure can never reach this branch.
		checks = winner
	}
	if checkErr != nil {
		out.Status = "checks_failed"
		if err := projectChecks(cfg, s.Branch, baseSHA, checks); err != nil {
			return out, fmt.Errorf("checks: %v (projection: %w)", checkErr, retryableHostError(cfg, err))
		}
		return out, fmt.Errorf("checks: %w", checkErr)
	}

	// A failing check is cheap and certain; a review is expensive. Stop here and
	// let the Fixer repair the head, so no reviewer is paid to read broken code.
	if checks.Status != "pass" {
		out.Status = "checks_failed"
		if err := projectChecks(cfg, s.Branch, baseSHA, checks); err != nil {
			return out, fmt.Errorf("projection: %w", retryableHostError(cfg, err))
		}
		return out, nil
	}

	// Select can precede another checkout's write. Refresh both durable notes
	// after Checks and under Item admission before paying for a second review.
	if err := fetchNotes(repoDir); err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("notes: refresh before review: %w", flowNoteError(err))
	}
	verdict, found, err := readVerdict(repoDir, baseSHA)
	if err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("notes: %w", flowNoteError(err))
	}
	if !found {
		var stats runStats
		// The durable winning Verdict decides whether the existing Host
		// preparation becomes pending. A losing review cannot publish intent.
		verdict, stats, err = verifierReview(repoDir, wtDir, it, baseSHA, runID, a)
		out.addTokens(stats)
		if err != nil {
			// A mechanical prompt-delivery failure names itself prompt_failed so it
			// is never misread as a review verdict the Fixer should repair; the
			// same prompt fails identically, so it parks instead. A review run
			// that exceeded its declared deadline parks the same way
			// (timeout_failed), never as a verdict to act on.
			out.Status = "review_failed"
			if isPromptDelivery(err) {
				out.Status = "prompt_failed"
			}
			if isRunTimeout(err) {
				out.Status = "timeout_failed"
			}
			return out, err
		}
	}
	out.Verdict = verdict.Verdict
	// Host retirement becomes pending only after its exact-Revision decision is
	// visible. A malformed reconciliation therefore cannot be bypassed by the
	// durable recovery path.
	if verdict.Verdict == "approve" && cfg.Projection.MergeViaHost {
		if err := recordHostRetirement(
			cfg, repoDir, s.Branch, baseSHA, it, verdict, checks,
		); err != nil {
			out.Status = "merge_failed"
			return out, fmt.Errorf("merge: record Host retirement: %w",
				retirementProjectionError(cfg, err))
		}
	} else if err := projectVerdict(cfg, s.Branch, baseSHA, verdict, checks); err != nil {
		out.Status = "projection_failed"
		return out, fmt.Errorf("projection: %w", retirementProjectionError(cfg, err))
	}
	if verdict.Verdict != "approve" {
		out.Status = "reviewed"
		return out, nil
	}
	// The durable Verdict owns merge attribution, including when another
	// checkout won the write-once race after this pass began.
	attemptKey := "branch-" + s.Branch
	attempts, err := readAttempts(repoDir, attemptKey)
	if err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("attempts: %w", err)
	}
	if why := mergeBlocked(cfg, attempts); why != "" {
		out.Status = "reviewed"
		return out, nil
	}
	mergeAgent := *a
	mergeAgent.Name = verdict.Reviewer
	mergeAgent.Model = verdict.Model
	mergeAgent.DefSHA = verdict.DefSHA
	if err := mergeVerified(cfg, repoDir, s.Branch, baseSHA, it, &mergeAgent); err != nil {
		switch {
		case errors.Is(err, errHostMergePending):
			out.Status = "merge_pending"
			return out, nil
		case errors.Is(err, errFlowRetryable):
			out.Status = "merge_pending"
			return out, err
		case errors.Is(err, errRetirementStale):
			out.Status = "stale"
			return out, err
		case errors.Is(err, errRetirementEvidenceInvalid),
			errors.Is(err, errAttemptsInvalid),
			errors.Is(err, errControlEvidenceInvalid),
			errors.Is(err, errHostMergeUnavailable):
			out.Status = "merge_failed"
			return out, err
		}
		// A branch that cannot land needs a human, not repeated external effects.
		// Publish the exact-Revision signal before the attempt fact, then reconcile
		// accepted-response loss through the Tracker read.
		out.Status = "merge_failed"
		tracker := trackerFor(cfg.Repo)
		if tagErr := tracker.SetTags(it.ID, []string{failedLabel}, nil); tagErr != nil {
			return out, fmt.Errorf("%w: merge failed: %v; set failed tag: %v",
				errFlowRetryable, err, tagErr)
		}
		if commentErr := publishMergeBlocked(tracker, it, baseSHA, err); commentErr != nil {
			return out, fmt.Errorf("%w: merge failed: %v; publish blocked comment: %v",
				errFlowRetryable, err, commentErr)
		}
		if _, berr := bumpAttempts(repoDir, attemptKey); berr != nil {
			return out, fmt.Errorf("merge: %w (attempt record failed: %v)", err, berr)
		}
		return out, err
	}
	out.Status = "merged"
	return out, nil
}

// verifierReview reviews one head and records the verdict as a note on it. It
// returns the phase statistics so the ledger reports the work the review cost
// in tokens; a discarded count makes every review look free.
func verifierReview(repoDir, wtDir string, it Item, head, runID string, a *Agent) (verdictNote, runStats, error) {
	var out verdictNote
	var stats runStats
	diff, err := gitOut(wtDir, "diff", "origin/master..."+head)
	if err != nil {
		return out, stats, fmt.Errorf("review: diff: %w", err)
	}
	prompt, err := renderUserPrompt(a, reviewData(it, report{}, diff))
	if err != nil {
		return out, stats, fmt.Errorf("review: prompt: %w", err)
	}
	// Snapshot the tracked worktree before the agent runs so a review that edits
	// any tracked file, stages a change, or moves HEAD can be refused on full
	// comparison with this state. The snapshot records content and index state,
	// so an edit to a file a check already dirtied is caught too.
	before, err := snapshotReviewTree(wtDir)
	if err != nil {
		return out, stats, fmt.Errorf("review: pre-run snapshot: %w", err)
	}
	trace := filepath.Join(workspaceDir(repoDir), "runs", runID+".verifier.jsonl")
	stats, phaseErr := runPhase(repoDir, wtDir, a, prompt, trace)
	// The worktree started at the Review revision; refuse a Verdict if the review
	// edited a tracked file or moved HEAD since the snapshot, naming what changed.
	// This assertion runs even when the phase crashed or timed out, so a verifier
	// that edits a tracked file and then fails is refused for the edit — the
	// required named clean-tree refusal — rather than reported only with the
	// harness error, which would let the mutation slip through unexamined.
	if err := assertCleanReviewTree(wtDir, head, before); err != nil {
		return out, stats, err
	}
	if phaseErr != nil {
		return out, stats, fmt.Errorf("review: %w", phaseErr)
	}
	rv, err := gateReview(wtDir, filepath.Join(repoDir, DefaultAgentsDir, a.Name, "report.schema.json"))
	if err != nil {
		return out, stats, fmt.Errorf("review: %w", err)
	}
	out = verdictNote{
		Verdict: rv.Verdict, Notes: rv.Notes, Reviewer: a.Name, Model: a.Model,
		DefSHA: a.DefSHA, RunID: runID, Time: nowRFC(),
	}
	if err := writeVerdict(repoDir, head, out); err != nil {
		if !errors.Is(err, errNoteExists) {
			return verdictNote{}, stats, fmt.Errorf("notes: %w", flowNoteError(err))
		}
		winner, found, readErr := readVerdict(repoDir, head)
		if readErr != nil {
			return verdictNote{}, stats, fmt.Errorf("notes: read winning verdict: %w", flowNoteError(readErr))
		}
		if !found {
			return verdictNote{}, stats, fmt.Errorf("notes: verdict note disappeared: %w", err)
		}
		return winner, stats, nil
	}
	return out, stats, nil
}

// rebaseOntoMaster moves the branch checked out in wtDir onto origin/master when
// the branch is behind it, then pushes the rebased branch with Git's
// compare-and-swap flag, so every later step keys to the tree that will land.
// A branch that is already current is left untouched and its head returned
// unchanged. A rebase that conflicts returns an error naming the conflicting
// paths, never a bare exit status.
func rebaseOntoMaster(wtDir, branch, expectedHead string, id CommitIdentity) (string, error) {
	if err := git(wtDir, "fetch", "origin", "master"); err != nil {
		return "", fmt.Errorf("rebase: fetch origin/master: %w", err)
	}
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rebase: head: %w", err)
	}
	behind, err := gitOut(wtDir, "rev-list", "--count", "origin/master", "^HEAD")
	if err != nil {
		return "", fmt.Errorf("rebase: compare: %w", err)
	}
	if behind == "0" {
		return head, nil
	}
	if err := gitAsCommitter(wtDir, id, "rebase", "origin/master"); err != nil {
		if paths, perr := gitOut(wtDir, "diff", "--name-only", "--diff-filter=U"); perr == nil && strings.TrimSpace(paths) != "" {
			return "", fmt.Errorf("rebase onto origin/master conflicts in %s", strings.Join(strings.Fields(paths), ", "))
		}
		return "", fmt.Errorf("rebase onto origin/master: %w", err)
	}
	head, err = gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rebase: head: %w", err)
	}
	if err := git(wtDir, "push", "--force-with-lease=refs/heads/"+branch+":"+expectedHead,
		"origin", "HEAD:"+branch); err != nil {
		return "", fmt.Errorf("rebase: push %s: %w", branch, err)
	}
	return head, nil
}

// fenceMergeOnRevision refuses a merge when the remote branch no longer points
// at the Revision that carried the approving Verdict.
func fenceMergeOnRevision(repoDir, branch, reviewed string) error {
	if err := git(repoDir, "fetch", "origin", branch); err != nil {
		return fmt.Errorf("merge: fetch %s: %w", branch, err)
	}
	observed, err := gitOut(repoDir, "rev-parse", "origin/"+branch)
	if err != nil {
		return fmt.Errorf("merge: observed tip of %s: %w", branch, err)
	}
	if observed != reviewed {
		return fmt.Errorf("merge refused: branch %s advanced to %s after its approving Verdict on reviewed Revision %s; re-review the new head", branch, observed, reviewed)
	}
	return nil
}
