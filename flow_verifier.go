package main

import (
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
	var fresh, mergeable []Subject
	for _, branch := range branches {
		head, err := branchHead(repoDir, branch)
		if err != nil {
			return nil, fmt.Errorf("branch %s: %w", branch, err)
		}
		s := Subject{
			Key:      "branch-" + branch,
			Kind:     "branch",
			Revision: head,
			Label:    branch,
			Issue:    itemNumberFromBranch(branch),
			Branch:   branch,
			Head:     head,
		}
		v, found, err := readVerdict(repoDir, head)
		if err != nil {
			return nil, fmt.Errorf("verdict %s: %w", branch, err)
		}
		if !found {
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
		// A branch that already spent its attempts could not land and now waits
		// for a human. Offering it again would retry one failure forever.
		attempts, err := readAttempts(repoDir, s.Key)
		if err != nil {
			return nil, fmt.Errorf("attempts %s: %w", branch, err)
		}
		if attempts >= cfg.Flows.Fixer.Attempts {
			continue
		}
		mergeable = append(mergeable, s)
	}
	return append(fresh, mergeable...), nil
}

func (verifierFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	it, err := getItem(cfg.Repo, s.Issue)
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
	defer func() {
		removeWorktree(repoDir, wtDir)
		untrackWorktree(wtDir)
	}()
	out := Outcome{Branch: s.Branch, BaseSHA: baseSHA, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA}

	// Rebase the branch onto current master before checking or reviewing it: a
	// Verdict and its checks must key to the exact tree that will land, not to a
	// tree built from an ancient master. Every later step uses the returned head.
	if newHead, err := rebaseOntoMaster(wtDir, s.Branch); err != nil {
		// A branch that cannot move onto current master can never land through
		// this factory. Spend one attempt, as the merge-failure path does, so the
		// verifier stops offering it, and say the reason on the item for a human.
		out.Status = "merge_failed"
		if _, berr := bumpAttempts(repoDir, s.Key); berr != nil {
			return out, fmt.Errorf("rebase: %w (attempt record failed: %v)", err, berr)
		}
		_ = labelItem(cfg.Repo, it.Number, []string{failedLabel}, nil)
		_ = commentItem(cfg.Repo, it.Number, "Merge blocked: "+err.Error())
		return out, fmt.Errorf("rebase: %w", err)
	} else {
		baseSHA = newHead
		// The ledger must record the head the checks, the Verdict, and the merge
		// all key to; the init value above was the pre-rebase head.
		out.BaseSHA = newHead
	}

	checks, checkErr := runChecks(cfg, wtDir, runID)
	if err := writeChecks(repoDir, baseSHA, checks); err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("notes: %w", err)
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
		verdict, stats, err = verifierReview(cfg, repoDir, wtDir, it, baseSHA, runID, a)
		out.TokIn, out.TokOut = stats.tokensIn, stats.tokensOut
		if err != nil {
			out.Status = "review_failed"
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
	if verdict.Verdict != "approve" || !cfg.Flows.Verifier.AutoMerge {
		out.Status = "reviewed"
		return out, nil
	}
	if err := mergeVerified(cfg, repoDir, s.Branch, it); err != nil {
		// A branch that cannot land needs a human, not another attempt. Spend one
		// attempt so the merge selector stops offering it, and say so on the item.
		out.Status = "merge_failed"
		if _, berr := bumpAttempts(repoDir, s.Key); berr != nil {
			return out, fmt.Errorf("merge: %w (attempt record failed: %v)", err, berr)
		}
		_ = labelItem(cfg.Repo, it.Number, []string{failedLabel}, nil)
		_ = commentItem(cfg.Repo, it.Number, "Merge blocked: "+err.Error())
		return out, err
	}
	out.Status = "merged"
	return out, nil
}

// verifierReview reviews one head and records the verdict as a note on it. It
// returns the phase statistics so the ledger reports the work the review cost
// in tokens; a discarded count makes every review look free.
func verifierReview(cfg Config, repoDir, wtDir string, it issue, head, runID string, a *Agent) (verdictNote, runStats, error) {
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
	trace := filepath.Join(workspaceDir(repoDir), "runs", runID+".verifier.jsonl")
	stats, err = runPhase(wtDir, a, prompt, trace, time.Duration(a.BudgetSec)*time.Second)
	if err != nil {
		return out, stats, fmt.Errorf("review: %w", err)
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
		return verdictNote{}, stats, fmt.Errorf("notes: %w", err)
	}
	return out, stats, nil
}

// rebaseOntoMaster moves the branch checked out in wtDir onto origin/master when
// the branch is behind it, then pushes the rebased branch with --force-with-lease,
// so every later step keys to the tree that will land. A branch that is already
// current is left untouched and its head returned unchanged. A rebase that
// conflicts returns an error naming the conflicting paths, never a bare exit
// status.
func rebaseOntoMaster(wtDir, branch string) (string, error) {
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
	if err := git(wtDir, "rebase", "origin/master"); err != nil {
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

// mergeVerified lands an approved branch. The host path and the git path are
// exclusive: a protected target branch means only the host may write it, and
// building a local commit that is then discarded would waste the work and
// confuse the next reader.
func mergeVerified(cfg Config, repoDir, branch string, it issue) error {
	if cfg.Projection.MergeViaHost {
		if err := projectMerge(cfg, branch, cfg.Flows.Verifier.Merge); err != nil {
			return fmt.Errorf("merge: projection: %w", err)
		}
		return finishMerge(cfg, repoDir, branch, it)
	}
	workspace := workspaceDir(repoDir)
	mergeDir := filepath.Join(workspace, "worktrees", "merge-"+slug(branch))
	trackWorktree(mergeDir)
	defer func() {
		removeWorktree(repoDir, mergeDir)
		untrackWorktree(mergeDir)
	}()
	_ = os.RemoveAll(mergeDir)
	if err := git(repoDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("merge: prune: %w", err)
	}
	if err := git(repoDir, "worktree", "add", "--detach", mergeDir, "origin/master"); err != nil {
		return fmt.Errorf("merge: worktree: %w", err)
	}
	switch cfg.Flows.Verifier.Merge {
	case "squash":
		// One commit per subject on master is the history shape this factory
		// has always produced; the strategy is declared, never assumed.
		if err := git(mergeDir, "merge", "--squash", branch); err != nil {
			return fmt.Errorf("merge: squash: %w", err)
		}
		if err := gitCommit(mergeDir, fmt.Sprintf("forest: %s (#%d)", it.Title, it.Number)); err != nil {
			return fmt.Errorf("merge: commit: %w", err)
		}
	case "ff":
		if err := git(mergeDir, "merge", "--ff-only", branch); err != nil {
			return fmt.Errorf("merge: ff: %w", err)
		}
	default:
		return fmt.Errorf("merge: unsupported strategy %q", cfg.Flows.Verifier.Merge)
	}
	if err := git(mergeDir, "push", "origin", "HEAD:master"); err != nil {
		return fmt.Errorf("merge: push: %w", err)
	}
	return finishMerge(cfg, repoDir, branch, it)
}

// finishMerge retires a landed subject: the branch is gone and the item is
// closed, so no lane selects it again.
func finishMerge(cfg Config, repoDir, branch string, it issue) error {
	if err := git(repoDir, "push", "origin", "--delete", branch); err != nil {
		return fmt.Errorf("merge: delete branch: %w", err)
	}
	if err := closeItem(cfg.Repo, it.Number); err != nil {
		return fmt.Errorf("merge: close item: %w", err)
	}
	if err := dropAttempts(repoDir, "branch-"+branch); err != nil {
		return fmt.Errorf("merge: drop attempt record: %w", err)
	}
	return nil
}

func itemNumberFromBranch(branch string) int {
	name := strings.TrimPrefix(branch, "forest/")
	if i := strings.IndexByte(name, '-'); i >= 0 {
		name = name[:i]
	}
	n, _ := strconv.Atoi(name)
	return n
}
