package main

import (
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
		v, hasVerdict, err := readVerdict(repoDir, head)
		if err != nil {
			return nil, fmt.Errorf("verdict %s: %w", branch, err)
		}
		checks, hasChecks, err := readChecks(repoDir, head)
		if err != nil {
			return nil, fmt.Errorf("checks %s: %w", branch, err)
		}
		if !(hasVerdict && v.Verdict == "changes") && !(hasChecks && checks.Status == "fail") {
			continue
		}
		attempts, err := readAttempts(repoDir, key)
		if err != nil {
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
		subjects = append(subjects, Subject{
			Key:      key,
			Kind:     "branch",
			Revision: head,
			Label:    branch,
			Issue:    itemNumberFromBranch(branch),
			Branch:   branch,
			Head:     head,
		})
	}
	return subjects, nil
}

func (fixerFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	it, err := getItem(cfg.Repo, s.Issue)
	if err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: s.Head, Status: "item_failed"}, fmt.Errorf("item: %w", err)
	}
	a, err := loadAgent(repoDir, cfg.Flows.Fixer.Agent)
	if err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: s.Head, Status: "agent_failed"}, fmt.Errorf("agent: %w", err)
	}
	v, _, err := readVerdict(repoDir, s.Head)
	if err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: s.Head, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA, Status: "notes_failed"}, fmt.Errorf("notes: %w", err)
	}
	checks, _, err := readChecks(repoDir, s.Head)
	if err != nil {
		return Outcome{Branch: s.Branch, BaseSHA: s.Head, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA, Status: "notes_failed"}, fmt.Errorf("notes: %w", err)
	}
	request := fixerRevision(v, checks)

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
	prompt, err := renderUserPrompt(a, issueData(it, request))
	if err != nil {
		out.Status = "prompt_failed"
		return out, fmt.Errorf("prompt: %w", err)
	}
	trace := filepath.Join(workspace, "runs", runID+".fixer.jsonl")
	stats, err := runPhase(wtDir, a, prompt, trace)
	out.TokIn, out.TokOut = stats.tokensIn, stats.tokensOut
	if err != nil {
		out.Status = "agent_failed"
		return out, fmt.Errorf("agent: %w", err)
	}
	if _, _, err := gate(wtDir, baseSHA, cfg.Protected,
		filepath.Join(repoDir, DefaultAgentsDir, a.Name, "report.schema.json")); err != nil {
		out.Status = "gate_failed"
		return out, fmt.Errorf("gate: %w", err)
	}
	if err := commitAndPush(repoDir, wtDir, s.Branch, s.Head, cfg.Commit, it); err != nil {
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

func markFixerFailed(repo string, it issue) error {
	if err := labelItem(repo, it.Number, []string{failedLabel}, nil); err != nil {
		return err
	}
	for _, c := range it.Comments {
		if strings.Contains(c.Body, "forest:failed: attempts ceiling reached") {
			return nil
		}
	}
	return commentItem(repo, it.Number, "forest:failed: attempts ceiling reached; human review is required.")
}
