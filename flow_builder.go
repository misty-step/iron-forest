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
	items, failures, err := eligibleItemsAndFailures(cfg, repoDir)
	if err != nil {
		return nil, err
	}
	subjects := make([]Subject, 0, len(items)+len(failures))
	for _, it := range items {
		subject := Subject{
			Key:      "item-" + it.ID,
			Kind:     subjectItem,
			Revision: it.UpdatedAt,
			Label:    fmt.Sprintf("#%s %s", it.ID, it.Title),
			ID:       it.ID,
			Item:     it,
		}
		subject, include, err := subjectAfterBrake(repoDir, "builder", subject)
		if err != nil {
			return nil, fmt.Errorf("stalled %s: %w", subject.Key, err)
		}
		if include {
			subjects = append(subjects, subject)
		}
	}
	for _, failure := range failures {
		failure, include, err := subjectAfterBrake(repoDir, "builder", failure)
		if err != nil {
			return nil, fmt.Errorf("stalled %s: %w", failure.Key, err)
		}
		if include {
			subjects = append(subjects, failure)
		}
	}
	return subjects, nil
}
func eligibleBuilderItem(cfg Config, repoDir, id, revision string) (Item, bool, error) {
	items, err := eligibleItems(cfg, repoDir)
	if err != nil {
		return Item{}, false, err
	}
	for _, it := range items {
		if it.ID == id && it.UpdatedAt == revision {
			return it, true, nil
		}
	}
	return Item{}, false, nil
}

func (builderFlow) Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error) {
	if s.Failure != nil {
		return Outcome{Status: "evidence_failed", BaseSHA: s.Revision}, s.Failure
	}
	// The caller holds the canonical Item admission. Re-run the complete
	// Selector now so a Tracker or durable-fact change after Select cannot start
	// an agent on stale work.
	it, current, err := eligibleBuilderItem(cfg, repoDir, s.ID, s.Revision)
	if err != nil {
		return Outcome{Status: "item_failed"}, fmt.Errorf("revalidate item: %w", err)
	}
	if !current {
		return Outcome{Status: "stale", BaseSHA: s.Revision}, nil
	}
	stalled, err := stalledOn(repoDir, "builder", s.Key, s.Revision)
	if err != nil {
		return Outcome{Status: "notes_failed"}, fmt.Errorf("revalidate stalled %s: %w", s.Key, err)
	}
	if stalled {
		return Outcome{Status: "stale", BaseSHA: s.Revision}, nil
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
		// A run that exceeded its declared deadline is likewise mechanical: the
		// same run keeps exceeding the same bound, so it parks (timeout_failed)
		// rather than look like a rejected change to repair.
		out.Status = "agent_failed"
		if isRunTimeout(err) {
			out.Status = "timeout_failed"
		}
		return out, fmt.Errorf("agent: %w", err)
	}
	changed, rep, err := gate(wtDir, baseSHA, a.ReportSchema, trace)
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
	// Linearize publication after agent work and the Gate. A mutable Tracker
	// Revision selected before the agent cannot authorize an external push.
	it, current, err = eligibleBuilderItem(cfg, repoDir, s.ID, s.Revision)
	if err != nil {
		out.Status = "item_failed"
		return out, fmt.Errorf("revalidate item before publication: %w", err)
	}
	if !current {
		out.Status = "stale"
		return out, nil
	}
	stalled, err = stalledOn(repoDir, "builder", s.Key, s.Revision)
	if err != nil {
		out.Status = "notes_failed"
		return out, fmt.Errorf("revalidate stalled %s before publication: %w", s.Key, err)
	}
	if stalled {
		out.Status = "stale"
		return out, nil
	}
	publishedHead, err := commitAndPush(repoDir, wtDir, branch, "", a.Commit, it)
	if err != nil {
		out.Status = "publish_failed"
		return out, fmt.Errorf("publish: %w", err)
	}
	body := builderProjectionBody(it, rep, changed)
	out.Status = "built"
	warnEffect("Issue comment", publishBuiltComment(
		cfg, repoDir, it, branch, publishedHead,
	))
	if cfg.Projection.MergeViaHost {
		if _, err := recordPreparingHostRetirement(cfg, repoDir, branch, publishedHead, it); err != nil {
			out.Status = "projection_failed"
			return out, fmt.Errorf("projection preparation: %w", err)
		}
	}
	url, _, err := projectBranch(cfg, repoDir, it, branch, body, publishedHead)
	if err != nil {
		out.Status = "projection_failed"
		return out, fmt.Errorf("projection: %w", err)
	}
	out.PRURL = url
	return out, nil
}
func publishBuiltComment(cfg Config, repoDir string, it Item, branch, revision string) error {
	return publishTrackerComment(
		repoDir,
		trackerFor(cfg.Repo),
		it,
		"Tracker-builder-comment",
		revision,
		fmt.Sprintf("Built branch `%s`.", branch),
		"<!-- iron-forest:built revision="+revision+" -->",
	)
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
