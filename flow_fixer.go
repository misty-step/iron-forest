package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type fixerFlow struct{}

func (fixerFlow) Name() string { return "fixer" }

func (fixerFlow) Interval(cfg Config) time.Duration {
	return time.Duration(cfg.Flows.Fixer.IntervalSec) * time.Second
}

func (fixerFlow) Enabled(cfg Config) bool { return cfg.Flows.Fixer.Enabled }

func (fixerFlow) Select(cfg Config, repoDir string) ([]Subject, error) {
	updateGate.RLock()
	defer updateGate.RUnlock()
	if err := fetchNotes(repoDir); err != nil {
		return nil, fmt.Errorf("notes: %w", err)
	}
	branchSnapshot, err := readForestBranchSnapshot(repoDir)
	if err != nil {
		return nil, err
	}
	subjects := make([]Subject, 0, len(branchSnapshot.Failures)+len(branchSnapshot.Actionable))
	for _, branch := range branchSnapshot.Actionable {
		head, err := branchHead(repoDir, branch)
		if err != nil {
			return nil, fmt.Errorf("branch %s: %w", branch, err)
		}
		key := "branch-" + branch
		subject := Subject{
			Key:      key,
			Kind:     subjectBranch,
			Revision: head,
			Label:    branch,
			ID:       itemIDFromBranch(branch),
			Branch:   branch,
		}
		subject, include, err := subjectAfterBrake(repoDir, "fixer", subject)
		if err != nil {
			return nil, fmt.Errorf("stalled %s: %w", key, err)
		}
		if !include {
			continue
		}
		if subject.Failure != nil {
			subjects = append(subjects, subject)
			continue
		}
		attempts, err := readAttempts(repoDir, key)
		if err != nil {
			if errors.Is(err, errAttemptsInvalid) {
				subject.Failure = err
				subjects = append(subjects, subject)
				continue
			}
			return nil, fmt.Errorf("attempts %s: %w", branch, err)
		}
		if attempts >= cfg.Flows.Fixer.Attempts {
			subjects = append(subjects, subject)
			continue
		}
		v, hasVerdict, err := readVerdict(repoDir, head)
		if err != nil {
			if errors.Is(err, errNoteInvalid) {
				subject.Failure = flowNoteError(err)
				subjects = append(subjects, subject)
				continue
			}
			return nil, fmt.Errorf("verdict %s: %w", branch, err)
		}
		checks, hasChecks, err := readChecks(repoDir, head)
		if err != nil {
			if errors.Is(err, errNoteInvalid) {
				subject.Failure = flowNoteError(err)
				subjects = append(subjects, subject)
				continue
			}
			return nil, fmt.Errorf("checks %s: %w", branch, err)
		}
		if !fixerNeedsRepair(v, hasVerdict, checks, hasChecks) {
			continue
		}
		subjects = append(subjects, subject)
	}
	for _, failure := range branchSnapshot.Failures {
		failure, include, err := subjectAfterBrake(repoDir, "fixer", failure)
		if err != nil {
			return nil, fmt.Errorf("stalled %s: %w", failure.Key, err)
		}
		if include {
			subjects = append(subjects, failure)
		}
	}
	return subjects, nil
}

func (fixerFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	out := Outcome{Branch: s.Branch, BaseSHA: s.Revision}
	if s.Failure != nil {
		out.Status = "evidence_failed"
		return out, s.Failure
	}
	if err := requalifyForestBranch(repoDir, s.Branch); err != nil {
		out.Status = "branch_failed"
		return out, err
	}
	head, present, err := lookupBranchHead(repoDir, s.Branch)
	if err != nil {
		out.Status = "branch_failed"
		return out, fmt.Errorf("branch: %w", err)
	}
	if !present || head != s.Revision {
		out.Status = "stale"
		out.BaseSHA = head
		return out, errSubjectRevisionStale
	}
	stalled, err := stalledOn(repoDir, "fixer", s.Key, s.Revision)
	if err != nil {
		out.Status = "evidence_failed"
		return out, err
	}
	if stalled {
		out.Status = "skipped"
		return out, nil
	}
	attempts, err := readAttempts(repoDir, s.Key)
	if err != nil {
		out.Status = "attempts_failed"
		return out, err
	}
	it, err := validatedTrackerItem(trackerFor(cfg.Repo), s.ID)
	if err != nil {
		out.Status = "item_failed"
		return out, fmt.Errorf("item: %w", err)
	}
	if itemClosed(it) {
		return stopClosedItem(repoDir, "fixer", s)
	}
	exhausted := attempts >= cfg.Flows.Fixer.Attempts
	if exhausted {
		if err := markFixerFailed(cfg.Repo, repoDir, s.Revision, it); err != nil {
			out.Status = "tracker_failed"
			return out, fmt.Errorf("tracker: %w", err)
		}
	}
	if err := fetchNotes(repoDir); err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("notes: refresh before repair: %w", flowNoteError(err))
	}
	v, hasVerdict, err := readVerdict(repoDir, s.Revision)
	if err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("notes: %w", flowNoteError(err))
	}
	checks, hasChecks, err := readChecks(repoDir, s.Revision)
	if err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("notes: %w", flowNoteError(err))
	}
	if hasChecks && checks.Status == "fail" {
		if err := projectChecks(cfg, repoDir, s.Branch, s.Revision, checks); err != nil {
			out.Status = "projection_failed"
			return out, fmt.Errorf("projection: %w", retryableHostError(cfg, err))
		}
	}
	if exhausted {
		if err := recordTerminalStall(repoDir, "fixer", s.Key, s.Revision); err != nil {
			out.Status = "evidence_failed"
			return out, fmt.Errorf("record failed handoff brake: %w", err)
		}
		out.Status = "exhausted"
		return out, nil
	}
	if !fixerNeedsRepair(v, hasVerdict, checks, hasChecks) {
		out.Status = "skipped"
		return out, nil
	}

	a, err := loadAgent(repoDir, cfg.Flows.Fixer.Agent)
	if err != nil {
		out.Status = "agent_failed"
		return out, fmt.Errorf("agent: %w", err)
	}
	request := fixerRevision(v, checks)

	workspace := workspaceDir(repoDir)
	wtDir, baseSHA, err := createWorktreeAtBranch(repoDir, workspace, s.Branch)
	if err != nil {
		out.Status = "worktree_failed"
		return out, fmt.Errorf("worktree: %w", err)
	}
	defer cleanupWorktree(repoDir, wtDir)
	if err := checkSubjectRevision(s, baseSHA); err != nil {
		return Outcome{
			Branch: s.Branch, BaseSHA: baseSHA, Agent: a.Name, Model: a.Model,
			DefSHA: a.DefSHA, Status: "stale",
		}, err
	}
	out.Agent, out.Model, out.DefSHA = a.Name, a.Model, a.DefSHA
	prompt, err := renderUserPrompt(a, issueData(it, request))
	if err != nil {
		out.Status = "prompt_failed"
		return out, fmt.Errorf("prompt: %w", err)
	}
	trace := filepath.Join(workspace, "runs", runID+".fixer.jsonl")
	stats, err := runPhase(repoDir, wtDir, a, prompt, trace)
	out.addTokens(stats)
	if err != nil {
		// A mechanical prompt-delivery failure is not content to repair: the same
		// prompt fails identically, so it parks (prompt_failed) instead of
		// spending a Fixer attempt on an unchanged situation. A run that exceeded
		// its declared deadline is the same shape: the same run keeps exceeding
		// the same bound, so it parks (timeout_failed) rather than being repaired.
		out.Status = "agent_failed"
		if isPromptDelivery(err) {
			out.Status = "prompt_failed"
		}
		if isRunTimeout(err) {
			out.Status = "timeout_failed"
		}
		return out, fmt.Errorf("agent: %w", err)
	}
	if _, _, err := gate(wtDir, baseSHA,
		filepath.Join(repoDir, DefaultAgentsDir, a.Name, "report.schema.json"),
		trace); err != nil {
		out.Status = "gate_failed"
		return out, fmt.Errorf("gate: %w", err)
	}
	publishedHead, count, err := commitFixAndPush(
		repoDir, wtDir, s.Branch, s.Revision, s.Key, attempts, a.Commit, it,
	)
	if err != nil {
		out.Status = "publish_failed"
		return out, fmt.Errorf("publish: %w", err)
	}
	if count >= cfg.Flows.Fixer.Attempts {
		if err := markFixerFailed(cfg.Repo, repoDir, publishedHead, it); err != nil {
			out.Status = "tracker_failed"
			return out, fmt.Errorf("tracker: %w", err)
		}
		if err := recordTerminalStall(repoDir, "fixer", s.Key, publishedHead); err != nil {
			out.Status = "evidence_failed"
			return out, fmt.Errorf("record failed handoff brake: %w", err)
		}
	}
	out.Status = "fixed"
	return out, nil
}

func fixerNeedsRepair(v verdictNote, hasVerdict bool, c checksNote, hasChecks bool) bool {
	return hasVerdict && v.Verdict == "changes" || hasChecks && c.Status == "fail"
}

func fixerRevision(v verdictNote, c checksNote) string {
	var b strings.Builder
	if strings.TrimSpace(v.Notes) != "" {
		b.WriteString("Reviewer notes:\n")
		b.WriteString(strings.TrimSpace(v.Notes))
		b.WriteString("\n\n")
	}
	if c.Status != "" {
		b.WriteString(checksSummary(c))
		for _, result := range c.Results {
			if result.Code == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n\nFailing check %s output:\n%s", result.Name, result.Output)
		}
	}
	return strings.TrimSpace(b.String())
}

func markFixerFailed(repo, repoDir, revision string, it Item) error {
	// The Flow admission claim records intent before this idempotent tag Effect.
	tk := trackerFor(repo)
	if err := tk.SetTags(it.ID, []string{failedLabel}, nil); err != nil {
		return err
	}
	marker := "<!-- iron-forest:fixer-failed revision=" + revision + " -->"
	return publishTrackerComment(
		repoDir,
		tk,
		it,
		"Tracker-fixer-comment",
		revision,
		"forest:failed: attempts ceiling reached; human review is required.",
		marker,
	)
}
