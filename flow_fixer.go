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
	branches, err := forestBranches(repoDir)
	if err != nil {
		return nil, err
	}
	var subjects []Subject
	for _, branch := range branches {
		head, err := branchHead(repoDir, branch)
		if err != nil {
			return nil, fmt.Errorf("branch %s: %w", branch, err)
		}
		key := "branch-" + branch
		subject := Subject{Key: key,
			Kind:     subjectBranch,
			Revision: head,
			Label:    branch,
			ID:       itemIDFromBranch(branch),
			Branch:   branch}
		v, hasVerdict, err := readVerdict(repoDir, head)
		if err != nil {
			if errors.Is(err, errNoteInvalid) {
				stalled, stallErr := stalledOn(repoDir, "fixer", key, head)
				if stallErr != nil {
					return nil, fmt.Errorf("stalled %s: %w", key, stallErr)
				}
				if stalled {
					continue
				}
				subject.Failure = flowNoteError(err)
				subjects = append(subjects, subject)
				continue
			}
			return nil, fmt.Errorf("verdict %s: %w", branch, err)
		}
		checks, hasChecks, err := readChecks(repoDir, head)
		if err != nil {
			if errors.Is(err, errNoteInvalid) {
				stalled, stallErr := stalledOn(repoDir, "fixer", key, head)
				if stallErr != nil {
					return nil, fmt.Errorf("stalled %s: %w", key, stallErr)
				}
				if stalled {
					continue
				}
				subject.Failure = flowNoteError(err)
				subjects = append(subjects, subject)
				continue
			}
			return nil, fmt.Errorf("checks %s: %w", branch, err)
		}
		if !fixerNeedsRepair(v, hasVerdict, checks, hasChecks) {
			continue
		}
		attempts, err := readAttempts(repoDir, key)
		if err != nil {
			if errors.Is(err, errAttemptsInvalid) {
				stalled, stallErr := stalledOn(repoDir, "fixer", key, head)
				if stallErr != nil {
					return nil, fmt.Errorf("stalled %s: %w", key, stallErr)
				}
				if !stalled {
					subject.Failure = err
					subjects = append(subjects, subject)
				}
				continue
			}
			return nil, fmt.Errorf("attempts %s: %w", branch, err)
		}
		if attempts >= cfg.Flows.Fixer.Attempts {
			continue
		}
		// The attempt ceiling bounds published repairs; this bounds the repairs
		// that never reached a commit, so a lane that cannot even publish stops
		// paying an agent to retry one unchanged situation.
		stalled, err := stalledOn(repoDir, "fixer", key, head)
		if err != nil {
			return nil, fmt.Errorf("stalled %s: %w", key, err)
		}
		if stalled {
			continue
		}
		subjects = append(subjects, subject)
	}
	return subjects, nil
}

func (fixerFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	out := Outcome{Branch: s.Branch, BaseSHA: s.Revision}
	if s.Failure != nil {
		out.Status = "notes_failed"
		return out, s.Failure
	}
	it, err := validatedTrackerItem(trackerFor(cfg.Repo), s.ID)
	if err != nil {
		out.Status = "item_failed"
		return out, fmt.Errorf("item: %w", err)
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
	if _, err := commitAndPush(repoDir, wtDir, s.Branch, s.Revision, a.Commit, it); err != nil {
		out.Status = "publish_failed"
		return out, fmt.Errorf("publish: %w", err)
	}
	count, err := bumpAttempts(repoDir, s.Key)
	if err != nil {
		out.Status = "attempts_failed"
		return out, fmt.Errorf("attempts: %w", err)
	}
	if count >= cfg.Flows.Fixer.Attempts {
		if err := markFixerFailed(cfg.Repo, it); err != nil {
			out.Status = "tracker_failed"
			return out, fmt.Errorf("tracker: %w", err)
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

func markFixerFailed(repo string, it Item) error {
	tk := trackerFor(repo)
	if err := tk.SetTags(it.ID, []string{failedLabel}, nil); err != nil {
		return err
	}
	for _, c := range it.Comments {
		if strings.Contains(c.Body, "forest:failed: attempts ceiling reached") {
			return nil
		}
	}
	return tk.Comment(it.ID, "forest:failed: attempts ceiling reached; human review is required.")
}
