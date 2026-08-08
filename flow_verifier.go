package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
		recoveries = append(recoveries, Subject{
			Key: "branch-" + record.Branch, Kind: "retirement",
			Revision: record.Revision, Label: "retire " + record.Branch,
			ID: record.ItemID, Branch: record.Branch, Head: record.Revision,
			Item: Item{ID: record.ItemID, Title: record.Title},
		})
	}
	var fresh, mergeable []Subject
	for _, branch := range branches {
		head := branchHeads[branch]
		if retiring[branch] {
			continue
		}
		s := Subject{
			Key:      "branch-" + branch,
			Kind:     "branch",
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
		// An approved, green branch is only a subject when it can actually land.
		// Every reason it cannot is named by mergeBlocked, so a branch waiting on
		// an operator is a state to read, not an action to run.
		attempts, err := readAttempts(repoDir, s.Key)
		if err != nil {
			return nil, fmt.Errorf("attempts %s: %w", branch, err)
		}
		if mergeBlocked(cfg, attempts) != "" {
			continue
		}
		mergeable = append(mergeable, s)
	}
	recoveries = append(recoveries, fresh...)
	recoveries = append(recoveries, mergeable...)
	return recoveries, nil
}

// mergeBlocked names why an approved, green branch may not land now, or returns
// "" when it may. It is the single authority for merge policy: Select consults
// it so a lane never offers a subject whose only possible outcome is a no-op,
// and Act consults the same function so the two cannot drift apart.
//
// Splitting this decision is what produced a live hot loop: Select checked the
// verdict, the checks and the attempts, Act checked auto_merge, and an approved
// branch under auto_merge: false was selected, rebased, rechecked and reviewed
// on every pass forever. 217 passes wrote 217 identical ledger rows and ran
// build, vet and test 217 times before discovering the merge was never allowed.
// A precondition that lives in one place cannot cause that.
func mergeBlocked(cfg Config, attempts int) string {
	if !cfg.Flows.Verifier.AutoMerge {
		return "auto_merge is off; an operator merges this branch"
	}
	if attempts >= cfg.Flows.Fixer.Attempts {
		return "merge attempts exhausted; a human is required"
	}
	return ""
}

var errRetirementStale = errors.New("retirement intent is stale")

func (verifierFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	if s.Kind == "retirement" {
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
		if err := recoverRetirementFact(cfg, repoDir, fact, s.Item); err != nil {
			switch {
			case errors.Is(err, errHostMergePending):
				out.Status = "merge_pending"
				return out, nil
			case errors.Is(err, errRetirementStale):
				out.Status = "stale"
				return out, nil
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
	a, err := loadAgent(repoDir, cfg.Flows.Verifier.Agent)
	if err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: s.Head, Status: "agent_failed"}, fmt.Errorf("agent: %w", err)
	}
	workspace := workspaceDir(repoDir)
	wtDir, baseSHA, err := createWorktreeAtBranch(repoDir, workspace, s.Branch)
	if err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: s.Head, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA, Status: "worktree_failed"}, fmt.Errorf("worktree: %w", err)
	}
	defer cleanupWorktree(repoDir, wtDir)
	out := Outcome{Branch: s.Branch, BaseSHA: baseSHA, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA}
	url, hostMerged, err := projectBranch(cfg, it, s.Branch,
		fmt.Sprintf("Recovered Projection for item #%s: %s.\n", it.ID, it.Title), baseSHA)
	if err != nil {
		out.Status = "projection_failed"
		return out, fmt.Errorf("projection: %w", err)
	}
	out.PRURL = url
	if hostMerged {
		verdict, hasVerdict, verr := readVerdict(repoDir, baseSHA)
		checks, hasChecks, cerr := readChecks(repoDir, baseSHA)
		if verr != nil || cerr != nil || !hasVerdict || !hasChecks ||
			verdict.Verdict != "approve" || checks.Status != "pass" {
			out.Status = "merge_failed"
			return out, fmt.Errorf("Host merged Revision %s without durable factory approval and passing Checks", baseSHA)
		}
		out.Verdict = verdict.Verdict
		if err := mergeVerified(cfg, repoDir, s.Branch, baseSHA, it, a); err != nil {
			if errors.Is(err, errHostMergePending) {
				out.Status = "merge_pending"
				return out, nil
			}
			out.Status = "merge_failed"
			return out, err
		}
		out.Status = "merged"
		return out, nil
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
	// A fresh review that lands on approve reaches here; a branch selected for
	// merge already passed this test in Select. Consulting the same authority
	// keeps the two from drifting, which is what caused the 217-pass hot loop.
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
			return out, nil
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

func mergeHostPath(cfg Config, repoDir, branch, reviewed string, it Item, a *Agent) error {
	fact, err := recordRetirement(repoDir, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: it.ID, Transport: "host",
		Strategy: cfg.Flows.Verifier.Merge, Title: it.Title, State: "pending",
		Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA,
	})
	if err != nil {
		return err
	}
	if err := projectMerge(cfg, branch, cfg.Flows.Verifier.Merge, reviewed); err != nil {
		if errors.Is(err, errHostMergeUnavailable) {
			if dropErr := dropRetirement(repoDir, fact); dropErr != nil {
				return fmt.Errorf("merge: projection stale: %v; drop pending retirement: %w", err, dropErr)
			}
			return fmt.Errorf("%w: %v", errRetirementStale, err)
		}
		return fmt.Errorf("%w: %v", errHostMergePending, err)
	}
	fact, err = landRetirement(repoDir, fact)
	if err != nil {
		return err
	}
	return finishRetirement(cfg, repoDir, fact, it)
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
		Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA,
	})
	if err != nil {
		return err
	}
	if err := git(mergeDir, "push", "--atomic",
		"--force-with-lease=refs/heads/master:"+masterTip,
		"--force-with-lease=refs/heads/"+branch+":"+reviewed,
		"--force-with-lease="+fact.Ref+":",
		"origin", "HEAD:master", reviewed+":refs/heads/"+branch, fact.SHA+":"+fact.Ref); err != nil {
		return fmt.Errorf("merge: push: %w", err)
	}
	return finishRetirement(cfg, repoDir, fact, it)
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

func recoverRetirementFact(cfg Config, repoDir string, fact retirementFact, it Item) error {
	record := fact.Record
	if record.ItemID != it.ID || record.Branch == "" || record.Revision == "" {
		return fmt.Errorf("retirement %s does not match item %q", fact.Ref, it.ID)
	}
	if record.State == "pending" {
		if record.Transport != "host" {
			return fmt.Errorf("retirement %s is pending without Host transport", fact.Ref)
		}
		err := projectMerge(cfg, record.Branch, record.Strategy, record.Revision)
		if err != nil {
			if errors.Is(err, errHostMergeUnavailable) {
				if dropErr := dropRetirement(repoDir, fact); dropErr != nil {
					return fmt.Errorf("merge: stale Host retirement: %v; drop intent: %w", err, dropErr)
				}
				return fmt.Errorf("%w: %v", errRetirementStale, err)
			}
			return fmt.Errorf("%w: %v", errHostMergePending, err)
		}
		var landErr error
		fact, landErr = landRetirement(repoDir, fact)
		if landErr != nil {
			return landErr
		}
	}
	return finishRetirement(cfg, repoDir, fact, it)
}

func finishRetirement(cfg Config, repoDir string, fact retirementFact, it Item) error {
	record := fact.Record
	// The durable retirement fact replaces the source branch as recovery
	// evidence. Delete the exact reviewed branch first, so an advanced branch is
	// refused before the Tracker item closes and a failed Close stays resumable.
	if err := deleteReviewedBranch(repoDir, record.Branch, record.Revision); err != nil {
		return err
	}
	// The marker excludes the item from Builder selection until both Tracker
	// retirement and its attempt cleanup finish.
	if err := trackerFor(cfg.Repo).Close(it.ID); err != nil {
		return fmt.Errorf("merge: close item: %w", err)
	}
	if err := dropAttempts(repoDir, "branch-"+record.Branch); err != nil {
		return fmt.Errorf("merge: drop attempt record: %w", err)
	}
	return dropRetirement(repoDir, fact)
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
		return fmt.Errorf("merge: branch %s advanced to %s before retirement of %s", branch, fields[0], reviewed)
	}
	if err := deleteRef(repoDir, "refs/heads/"+branch, reviewed); err != nil {
		return fmt.Errorf("merge: delete branch %s (wanted %s): %w", branch, reviewed, err)
	}
	return nil
}

// encodeBranchID renders a tracker id as a forest branch's id segment. The
// branch keeps the forest/<id>-<slug> shape so numeric GitHub ids read as they
// always have. The segment must be valid in a git refname and in a filesystem
// path, so every byte outside a small safe set is escaped as %XX; '%' itself is
// always escaped so the decoder can treat any '%' as the start of an escape.
// The delimiter on the way back is the first '-', so '-' is escaped too. Numeric
// ids and hyphen-free alphanumeric Habitat ids contain only safe bytes, so their
// branches are unchanged.
func encodeBranchID(id string) string {
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		if isBranchIDByte(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// isBranchIDByte reports whether c can appear literally in a forest branch's id
// segment. Only bytes valid in a git refname and in a file path are kept; '/' and
// other path separators, control bytes, whitespace, and git's special characters
// are escaped so any opaque id derives a usable worktree and branch.
func isBranchIDByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || c == '_'
}

// decodeBranchID reverses encodeBranchID in a single left-to-right pass. '%'
// always begins a two-hex-digit escape, so an id containing the literal escape
// sequence `%2D` (encoded as `%252D`) reconstructs to `%2D`, never to a stray
// '-'; the mapping is bijective and any opaque id round-trips.
func decodeBranchID(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// itemIDFromBranch recovers the opaque item identity from a forest branch,
// undoing encodeBranchID on the id segment. It never assumes the segment is an
// integer: it stays a numeric GitHub id or a Habitat id as written.
func itemIDFromBranch(branch string) string {
	name := strings.TrimPrefix(branch, BranchPrefix)
	if i := strings.IndexByte(name, '-'); i >= 0 {
		name = name[:i]
	}
	return decodeBranchID(name)
}
