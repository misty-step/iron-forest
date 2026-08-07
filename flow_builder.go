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
			Kind:     "item",
			Revision: it.UpdatedAt,
			Label:    fmt.Sprintf("#%s %s", it.ID, it.Title),
			ID:       it.ID,
			Item:     it,
		})
	}
	return subjects, nil
}

func (builderFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	// Re-read the current item at this effect boundary instead of trusting the
	// copy embedded in the Subject. The Subject may be stale: a label applied
	// between Select and Act (forest:failed, or any other change) must be
	// honored, because failed is terminal and a failed item must never be built
	// and published. This read is what the machine's observe() below sees.
	it, err := trackerFor(cfg.Repo).Get(s.ID)
	if err != nil {
		return Outcome{Status: "item_failed"}, fmt.Errorf("item: %w", err)
	}

	// The build effect is only legal on an eligible item: never a subject
	// another flow has already claimed. The state is derived from git-visible
	// facts -- whether a forest branch already covers this item and whether the
	// item carries the failure label -- so a second builder, a leftover branch,
	// or a concurrent claim is refused by the machine, never assumed away by a
	// hard-coded state.
	branches, err := forestBranches(repoDir)
	if err != nil {
		return Outcome{Status: "item_failed"}, fmt.Errorf("branches: %w", err)
	}
	hasBranch := false
	for _, b := range branches {
		if itemIDFromBranch(b) == it.ID {
			hasBranch = true
			break
		}
	}
	ffacts := subjectFacts{
		revision: s.Revision, hasBranch: hasBranch, itemOpen: it.Open,
		failedLabel: it.hasTag(failedLabel),
	}
	if _, err := transit(observe(ffacts), effectBuild, ffacts, "", "builder"); err != nil {
		return Outcome{Status: "item_failed"}, fmt.Errorf("build: %w", err)
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
	defer func() {
		removeWorktree(repoDir, wtDir)
		untrackWorktree(wtDir)
	}()

	prompt, err := renderUserPrompt(a, issueData(it, ""))
	if err != nil {
		return Outcome{Status: "prompt_failed", Branch: branch, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA, BaseSHA: baseSHA}, fmt.Errorf("prompt: %w", err)
	}
	trace := filepath.Join(workspace, "runs", runID+".builder.jsonl")
	stats, err := runPhase(repoDir, wtDir, a, prompt, trace)
	out := Outcome{
		Branch: branch, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA,
		BaseSHA: baseSHA, TokIn: stats.tokensIn, TokOut: stats.tokensOut,
	}
	if err != nil {
		out.Status = "agent_failed"
		return out, fmt.Errorf("agent: %w", err)
	}
	changed, rep, err := gate(wtDir, baseSHA,
		filepath.Join(repoDir, DefaultAgentsDir, a.Name, "report.schema.json"))
	if err != nil {
		out.Status = "gate_failed"
		return out, fmt.Errorf("gate: %w", err)
	}
	if err := commitAndPush(repoDir, wtDir, branch, "", cfg.Commit, it); err != nil {
		out.Status = "publish_failed"
		return out, fmt.Errorf("publish: %w", err)
	}
	// A publish lands a bare head: the subject returns to pushed, where no
	// Verdict or Checks note may be inherited.
	head, err := gitOut(repoDir, "rev-parse", "refs/remotes/origin/"+branch)
	if err != nil {
		out.Status = "publish_failed"
		return out, fmt.Errorf("publish: %w", err)
	}
	if _, err := transit(stateBuilding, effectPublish, subjectFacts{revision: head}, "", "builder"); err != nil {
		out.Status = "publish_failed"
		return out, fmt.Errorf("publish: %w", err)
	}
	body := builderProjectionBody(it, rep, changed)
	if err := trackerFor(cfg.Repo).Comment(it.ID, fmt.Sprintf("Built branch `%s`.", branch)); err != nil {
		out.Status = "comment_failed"
		return out, fmt.Errorf("comment: %w", err)
	}
	url, err := projectBranch(cfg, it, branch, body)
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
	return b.String()
}
