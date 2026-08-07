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
		subjects = append(subjects, Subject{
			Key:      fmt.Sprintf("item-%d", it.Number),
			Kind:     "item",
			Revision: it.UpdatedAt,
			Label:    fmt.Sprintf("#%d %s", it.Number, it.Title),
			Issue:    it.Number,
			Item:     it,
		})
	}
	return subjects, nil
}

func (builderFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	it := s.Item
	if it.Number == 0 {
		var err error
		it, err = getItem(cfg.Repo, s.Issue)
		if err != nil {
			return Outcome{Status: "item_failed"}, fmt.Errorf("item: %w", err)
		}
	}

	a, err := loadAgent(repoDir, cfg.Flows.Builder.Agent)
	if err != nil {
		return Outcome{Status: "agent_failed"}, fmt.Errorf("agent: %w", err)
	}
	workspace := workspaceDir(repoDir)
	wtDir, branch, baseSHA, err := createWorktree(repoDir, workspace, it)
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
	stats, err := runPhase(wtDir, a, prompt, trace, time.Duration(a.BudgetSec)*time.Second)
	out := Outcome{
		Branch: branch, Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA,
		BaseSHA: baseSHA, TokIn: stats.tokensIn, TokOut: stats.tokensOut,
	}
	if err != nil {
		out.Status = "agent_failed"
		return out, fmt.Errorf("agent: %w", err)
	}
	changed, rep, err := gate(wtDir, baseSHA, cfg.Protected,
		filepath.Join(repoDir, DefaultAgentsDir, a.Name, "report.schema.json"))
	if err != nil {
		out.Status = "gate_failed"
		return out, fmt.Errorf("gate: %w", err)
	}
	if err := commitAndPush(repoDir, wtDir, branch, "", cfg.Commit, it); err != nil {
		out.Status = "publish_failed"
		return out, fmt.Errorf("publish: %w", err)
	}
	body := builderProjectionBody(it, rep, changed)
	if err := commentItem(cfg.Repo, it.Number, fmt.Sprintf("Built branch `%s`.", branch)); err != nil {
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

func builderProjectionBody(it issue, rep report, changed []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Generated for item #%d: %s.\n\n", it.Number, it.Title)
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
