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
	retirements, err := listRetirements(repoDir)
	if err != nil {
		return nil, fmt.Errorf("retirements: %w", err)
	}
	var recoveries []Subject
	retiring := make(map[string]bool, len(retirements))
	for _, fact := range retirements {
		record := fact.Record
		retiring[record.Branch] = true
		s := Subject{
			Key: "branch-" + record.Branch, Kind: subjectRetirement,
			Revision: record.Revision, Label: "retire " + record.Branch,
			ID: record.ItemID, Branch: record.Branch, Head: record.Revision,
			Item: Item{ID: record.ItemID, Title: record.Title},
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
		s := Subject{
			Key:      "branch-" + branch,
			Kind:     subjectBranch,
			Revision: head,
			Label:    branch,
			ID:       itemIDFromBranch(branch),
			Branch:   branch,
			Head:     head,
		}
		stalled, err := stalledOn(repoDir, "verifier", s.Key, head)
		if err != nil {
			return nil, fmt.Errorf("stalled %s: %w", s.Key, err)
		}
		if stalled {
			continue
		}
		v, found, err := readVerdict(repoDir, head)
		if err != nil {
			return nil, fmt.Errorf("verdict %s: %w", branch, err)
		}
		if !found {
			// A head whose checks already failed belongs to the Fixer: rechecking
			// the same commit buys the same fact and pays for it again. A repaired
			// branch has a new head carrying no notes, so it returns here.
			checks, hasChecks, cerr := readChecks(repoDir, head)
			if cerr != nil {
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
			return nil, fmt.Errorf("checks %s: %w", branch, err)
		}
		if !found || checks.Status != "pass" {
			continue
		}
		// An approved, green branch is a subject when it can land. With Host
		// projection and AutoMerge disabled, select it once to prepare durable
		// intent; the resulting retirement fact suppresses later branch work.
		attempts, err := readAttempts(repoDir, s.Key)
		if err != nil {
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

func (verifierFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	if s.Kind == subjectRetirement {
		fact, found, err := readRetirement(repoDir, s.Branch, s.Revision)
		if err != nil {
			return Outcome{Branch: s.Branch, BaseSHA: s.Revision, Status: "merge_failed"}, err
		}
		if !found {
			return Outcome{Branch: s.Branch, BaseSHA: s.Revision, Status: "merge_failed"},
				fmt.Errorf("retirement for %s at %s disappeared", s.Branch, s.Revision)
		}
		record := fact.Record
		out := Outcome{
			Branch: s.Branch, BaseSHA: s.Revision,
			Agent: record.Agent, Model: record.Model, DefSHA: record.DefSHA,
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
	it, err := trackerFor(cfg.Repo).Get(s.ID)
	if err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: s.Head, Status: "item_failed"}, fmt.Errorf("item: %w", err)
	}
	workspace := workspaceDir(repoDir)
	wtDir, baseSHA, err := createWorktreeAtBranch(repoDir, workspace, s.Branch)
	if err != nil {
		if cfg.Projection.MergeViaHost && s.Head != "" {
			merged, pr, inspectErr := inspectProjectMerge(cfg, s.Branch, cfg.Flows.Verifier.Merge, s.Head)
			if inspectErr == nil && merged && pr.HeadRefOID == s.Head {
				out := Outcome{Branch: s.Branch, BaseSHA: s.Head, PRURL: pr.URL}
				return recoverHostMergedProjection(cfg, repoDir, s.Branch, s.Head, it, out)
			}
		}
		return Outcome{Branch: s.Branch, BaseSHA: s.Head, Status: "worktree_failed"}, fmt.Errorf("worktree: %w", err)
	}
	defer cleanupWorktree(repoDir, wtDir)
	a, err := loadAgent(repoDir, cfg.Flows.Verifier.Agent)
	if err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: s.Head, Status: "agent_failed"}, fmt.Errorf("agent: %w", err)
	}
	out := Outcome{Branch: s.Branch, BaseSHA: baseSHA, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA}
	url, hostMerged, err := projectBranch(cfg, it, s.Branch,
		fmt.Sprintf("Recovered Projection for item #%s: %s.\n", it.ID, it.Title), baseSHA)
	if err != nil {
		out.Status = "projection_failed"
		return out, fmt.Errorf("projection: %w", err)
	}
	out.PRURL = url
	if hostMerged {
		return recoverHostMergedProjection(cfg, repoDir, s.Branch, baseSHA, it, out)
	}
	// Rebase the branch onto current master before checking or reviewing it: a
	// Verdict and its checks must key to the exact tree that will land, not to a
	// tree built from an ancient master. Every later step uses the returned head.
	newHead, rebaseErr := rebaseOntoMaster(wtDir, s.Branch, a.Commit)
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
				return out, fmt.Errorf("rebase: %w (notes: %v)", rebaseErr, err)
			}
			winner, found, readErr := readChecks(repoDir, baseSHA)
			if readErr != nil {
				return out, fmt.Errorf("rebase: %w (notes: read winning check: %v)", rebaseErr, readErr)
			}
			if !found {
				return out, fmt.Errorf("rebase: %w (notes: check note disappeared: %v)", rebaseErr, err)
			}
			note = winner
		}
		if err := projectChecks(cfg, s.Branch, note); err != nil {
			return out, fmt.Errorf("rebase: %w (projection: %v)", rebaseErr, err)
		}
		return out, fmt.Errorf("rebase: %w", rebaseErr)
	}
	baseSHA = newHead
	// The ledger must record the head the checks, the Verdict, and the merge all
	// key to; the value above was the pre-rebase head.
	out.BaseSHA = newHead

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
	// A preflight failure means no declared check ran: the child environment
	// could not be built, the toolchain was missing, or FOREST_CHECK_PATH did
	// not resolve. There is nothing to record, so no Checks note exists, and the
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
			return out, fmt.Errorf("notes: %w", err)
		}
		winner, found, readErr := readChecks(repoDir, baseSHA)
		if readErr != nil {
			out.Status = "notes_failed"
			return out, fmt.Errorf("notes: read winning check: %w", readErr)
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
		return out, fmt.Errorf("checks: %w", checkErr)
	}

	// A failing check is cheap and certain; a review is expensive. Stop here and
	// let the Fixer repair the head, so no reviewer is paid to read broken code.
	if checks.Status != "pass" {
		out.Status = "checks_failed"
		if err := projectChecks(cfg, s.Branch, checks); err != nil {
			return out, fmt.Errorf("projection: %w", err)
		}
		return out, nil
	}

	verdict, found, err := readVerdict(repoDir, baseSHA)
	if err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("notes: %w", err)
	}
	if !found {
		var stats runStats
		// A new approval prepares Host retirement before this Verdict is
		// persisted. Reusing the callback keeps the preparation before the
		// projection comment and avoids a duplicate record after review.
		verdict, stats, err = verifierReview(repoDir, wtDir, it, baseSHA, runID, a,
			func(v verdictNote) error {
				return recordHostRetirement(cfg, repoDir, s.Branch, baseSHA, it, v)
			})
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
	if found && verdict.Verdict == "approve" {
		if err := recordHostRetirement(cfg, repoDir, s.Branch, baseSHA, it, verdict); err != nil {
			out.Status = "merge_failed"
			return out, fmt.Errorf("merge: record Host retirement: %w", err)
		}
	}
	out.Verdict = verdict.Verdict
	// Every terminal decision reaches the human surface, not merges alone: a
	// rejection is the outcome an operator most needs to see.
	if err := projectVerdict(cfg, s.Branch, verdict, checks); err != nil {
		out.Status = "projection_failed"
		return out, fmt.Errorf("projection: %w", err)
	}
	if verdict.Verdict != "approve" {
		out.Status = "reviewed"
		return out, nil
	}
	// A fresh review callback already prepared Host intent. An existing
	// approved branch reached this point only after Select admitted its work.
	// Consulting the same authority keeps the two from drifting apart.
	attempts, err := readAttempts(repoDir, s.Key)
	if err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("attempts: %w", err)
	}
	if why := mergeBlocked(cfg, attempts); why != "" {
		out.Status = "reviewed"
		return out, nil
	}
	if err := mergeVerified(cfg, repoDir, s.Branch, baseSHA, it, a); err != nil {
		if errors.Is(err, errHostMergePending) {
			out.Status = "merge_pending"
			return out, nil
		}
		if errors.Is(err, errRetirementStale) {
			out.Status = "stale"
			return out, err
		}
		// A branch that cannot land needs a human, not another attempt. Spend one
		// attempt so the merge selector stops offering it, and say so on the item.
		out.Status = "merge_failed"
		if _, berr := bumpAttempts(repoDir, s.Key); berr != nil {
			return out, fmt.Errorf("merge: %w (attempt record failed: %v)", err, berr)
		}
		trackerFor(cfg.Repo).SetTags(it.ID, []string{failedLabel}, nil)
		_ = trackerFor(cfg.Repo).Comment(it.ID, "Merge blocked: "+redactSecretShaped(err.Error()))
		return out, err
	}
	out.Status = "merged"
	return out, nil
}

// verifierReview reviews one head and records the verdict as a note on it. It
// returns the phase statistics so the ledger reports the work the review cost
// in tokens; a discarded count makes every review look free.
func verifierReview(repoDir, wtDir string, it Item, head, runID string, a *Agent, onApprove func(verdictNote) error) (verdictNote, runStats, error) {
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
	if out.Verdict == "approve" && onApprove != nil {
		if err := onApprove(out); err != nil {
			return verdictNote{}, stats, fmt.Errorf("retirement: %w", err)
		}
	}
	if err := writeVerdict(repoDir, head, out); err != nil {
		if !errors.Is(err, errNoteExists) {
			return verdictNote{}, stats, fmt.Errorf("notes: %w", err)
		}
		winner, found, readErr := readVerdict(repoDir, head)
		if readErr != nil {
			return verdictNote{}, stats, fmt.Errorf("notes: read winning verdict: %w", readErr)
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
func rebaseOntoMaster(wtDir, branch string, id CommitIdentity) (string, error) {
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
	if err := git(wtDir, "push", "--force-with-lease", "origin", "HEAD:"+branch); err != nil {
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
