package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type builderFlow struct{}

func (builderFlow) Name() string { return "builder" }

func (builderFlow) Interval(cfg Config) time.Duration {
	return time.Duration(cfg.Flows.Builder.IntervalSec) * time.Second
}

func (builderFlow) Enabled(cfg Config) bool { return cfg.Flows.Builder.Enabled }

func (builderFlow) Select(cfg Config, repoDir string) ([]Subject, error) {
	updateGate.RLock()
	defer updateGate.RUnlock()
	// eligibleItems already drops items covered by a forest branch, so this
	// selector must not repeat that rule and drift from it.
	items, err := eligibleItems(cfg, repoDir)
	if err != nil {
		return nil, err
	}
	var subjects []Subject
	for _, it := range items {
		// An unchanged situation that reached the failure limit is not work. The
		// key keeps its numeric shape for GitHub ids so the durable brake ref and
		// the subject key are unchanged.
		key := "item-" + it.ID
		stalled, err := stalledOn(repoDir, "builder", key, it.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("stalled %s: %w", key, err)
		}
		if stalled {
			continue
		}
		subjects = append(subjects, Subject{
			Key:      key,
			Kind:     subjectItem,
			Revision: it.UpdatedAt,
			Label:    fmt.Sprintf("#%s %s", it.ID, it.Title),
			ID:       it.ID,
			Item:     it,
		})
	}
	return subjects, nil
}

func (builderFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	it := s.Item
	if it.ID == "" {
		var err error
		it, err = trackerFor(cfg.Repo).Get(s.ID)
		if err != nil {
			return Outcome{Status: "item_failed"}, fmt.Errorf("item: %w", err)
		}
	}

	if cfg.Projection.Enabled && cfg.Projection.MergeViaHost {
		branches, err := forestBranches(repoDir)
		if err != nil {
			return Outcome{Status: "branch_failed"}, fmt.Errorf("branches: %w", err)
		}
		for _, branch := range branches {
			if itemIDFromBranch(branch) == it.ID {
				return Outcome{Status: "stale", BaseSHA: s.Revision}, nil
			}
		}
		retiring, err := retirementItemIDs(repoDir)
		if err != nil {
			return Outcome{Status: "branch_failed"}, fmt.Errorf("retirements: %w", err)
		}
		for _, id := range retiring {
			if id == it.ID {
				return Outcome{Status: "stale", BaseSHA: s.Revision}, nil
			}
		}
	}
	a, err := loadAgent(repoDir, cfg.Flows.Builder.Agent)
	if err != nil {
		return Outcome{Status: "agent_failed"}, fmt.Errorf("agent: %w", err)
	}
	workspace := workspaceDir(repoDir)
	wtDir, branch, baseSHA, err := createWorktree(repoDir, workspace, it.ID, it.Title)
	if err != nil {
		return Outcome{Status: "worktree_failed", Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA}, fmt.Errorf("worktree: %w", err)
	}
	defer cleanupWorktree(repoDir, wtDir)

	prompt, err := renderUserPrompt(a, issueData(it, ""))
	if err != nil {
		return Outcome{Status: "prompt_failed", Branch: branch, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA, BaseSHA: baseSHA}, fmt.Errorf("prompt: %w", err)
	}
	trace := filepath.Join(workspace, "runs", runID+".builder.jsonl")
	stats, err := runPhase(repoDir, wtDir, a, prompt, trace)
	out := Outcome{
		Branch: branch, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA,
		BaseSHA: baseSHA,
	}
	out.addTokens(stats)
	if err != nil {
		// A mechanical delivery failure is not a content or agent failure: the
		// same prompt keeps failing identically, so it must be named prompt_failed
		// and park rather than look like work a Fixer attempt could repair.
		// A run that exceeded its declared deadline is likewise mechanical: the
		// same run keeps exceeding the same bound, so it parks (timeout_failed)
		// rather than look like a rejected change to repair.
		out.Status = "agent_failed"
		if isPromptDelivery(err) {
			out.Status = "prompt_failed"
		}
		if isRunTimeout(err) {
			out.Status = "timeout_failed"
		}
		return out, fmt.Errorf("agent: %w", err)
	}
	changed, rep, err := gate(wtDir, baseSHA,
		filepath.Join(repoDir, DefaultAgentsDir, a.Name, "report.schema.json"),
		trace)
	if err != nil {
		out.Status = "gate_failed"
		return out, fmt.Errorf("gate: %w", err)
	}
	// Agent-authored prose is published verbatim into the pull-request body, so
	// a credential-shaped summary or note is a seam a secret could cross. Refuse
	// the whole run before any branch is pushed or any projection is opened, and
	// record it as blocked so an operator resolves the report instead of a broken
	// pull request reaching the host.
	if secretShaped(rep.Summary) || secretShaped(rep.Notes) {
		out.Status = "blocked"
		out.BaseSHA = baseSHA
		return out, fmt.Errorf("blocked: report carries credential-shaped prose; no branch or pull request published")
	}
	if err := commitAndPush(repoDir, wtDir, branch, "", a.Commit, it); err != nil {
		out.Status = "publish_failed"
		return out, fmt.Errorf("publish: %w", err)
	}
	body := builderProjectionBody(it, rep, changed)
	if err := trackerFor(cfg.Repo).Comment(it.ID, fmt.Sprintf("Built branch `%s`.", branch)); err != nil {
		out.Status = "comment_failed"
		return out, fmt.Errorf("comment: %w", err)
	}
	url, _, err := projectBranch(cfg, it, branch, body, "")
	if err != nil {
		out.Status = "projection_failed"
		return out, fmt.Errorf("projection: %w", err)
	}
	out.Status = "built"
	out.PRURL = url
	return out, nil
}

func builderProjectionBody(it Item, rep report, changed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Generated for item #%s: %s.\n\n", it.ID, it.Title)
	b.WriteString(rep.Summary)
	b.WriteString("\n\nChanged files:\n")
	for _, path := range changed {
		fmt.Fprintf(&b, "- %s\n", path)
	}
	if notes := strings.TrimSpace(rep.Notes); notes != "" && !strings.EqualFold(notes, "none") {
		fmt.Fprintf(&b, "\nNotes: %s\n", notes)
	}
	// Defense in depth on the projection body itself: even a path that does not
	// block still never ships a secret-shaped token verbatim.
	return redactSecretShaped(b.String())
}
