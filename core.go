package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/misty-step/iron-forest/core"
)

// core forwards every durable operation to the existing package functions. It
// is a forwarding layer on purpose: this card defines the seam, not the move.
// It implements core.API and adapts the controller's internal state into the
// core package's owned types.
type coreImpl struct {
	repoDir string
}

// NewCore returns the API for a checkout at repoDir, usable without a daemon.
func NewCore(repoDir string) core.API {
	return &coreImpl{repoDir: repoDir}
}

func (c *coreImpl) Config() (core.Config, error) {
	cfg, err := loadConfig(filepath.Join(c.repoDir, "forest.yaml"))
	if err != nil {
		return core.Config{}, err
	}
	checks := make([]core.Check, 0, len(cfg.Checks))
	for _, ch := range cfg.Checks {
		checks = append(checks, core.Check{Name: ch.Name, Run: ch.Run})
	}
	return core.Config{
		Repo:   cfg.Repo,
		Commit: core.CommitIdentity{Name: cfg.Commit.Name, Email: cfg.Commit.Email},
		Checks: checks,
		Flows: core.Flows{
			Builder: core.BuilderFlowConfig{
				FlowConfig:    flowConfig(cfg.Flows.Builder.Enabled, cfg.Flows.Builder.Agent, cfg.Flows.Builder.IntervalSec),
				ExcludeLabels: cfg.Flows.Builder.ExcludeLabels,
				RequireLabels: cfg.Flows.Builder.RequireLabels,
			},
			Verifier: core.VerifierFlowConfig{
				FlowConfig: flowConfig(cfg.Flows.Verifier.Enabled, cfg.Flows.Verifier.Agent, cfg.Flows.Verifier.IntervalSec),
				Merge:      cfg.Flows.Verifier.Merge,
				AutoMerge:  cfg.Flows.Verifier.AutoMerge,
			},
			Fixer: core.FixerFlowConfig{
				FlowConfig: flowConfig(cfg.Flows.Fixer.Enabled, cfg.Flows.Fixer.Agent, cfg.Flows.Fixer.IntervalSec),
				Attempts:   cfg.Flows.Fixer.Attempts,
			},
			Manager: core.ManagerFlowConfig{
				FlowConfig:  flowConfig(cfg.Flows.Manager.Enabled, cfg.Flows.Manager.Agent, cfg.Flows.Manager.IntervalSec),
				ReadyDepth:  cfg.Flows.Manager.ReadyDepth,
				ExcludeTags: cfg.Flows.Manager.ExcludeTags,
			},
		},
		Projection: core.ProjectionConfig{Enabled: cfg.Projection.Enabled, MergeViaHost: cfg.Projection.MergeViaHost},
	}, nil
}

func flowConfig(enabled bool, agent string, interval int) core.FlowConfig {
	return core.FlowConfig{Enabled: enabled, Agent: agent, IntervalSec: interval}
}

func (c *coreImpl) Agents() ([]core.AgentInfo, error) {
	names, err := discoverAgents(c.repoDir)
	if err != nil {
		return nil, err
	}
	out := make([]core.AgentInfo, 0, len(names))
	for _, name := range names {
		a, err := loadAgent(c.repoDir, name)
		if err != nil {
			// Keep listing the rest: one malformed declaration must not hide the
			// agents that follow it, matching what the agents command reported
			// before this seam existed. The caller prints the error and moves on.
			out = append(out, core.AgentInfo{Name: name, Err: err.Error()})
			continue
		}
		mcps := make([]core.McpSpec, 0, len(a.MCP))
		for _, m := range a.MCP {
			mcps = append(mcps, core.McpSpec{
				Name: m.Name, Type: m.Type, URL: m.URL, Header: m.Header, Enabled: m.Enabled,
			})
		}
		out = append(out, core.AgentInfo{
			Name: a.Name, Description: a.Description, Model: a.Model,
			Variant: a.Variant, Mode: a.Mode, DefSHA: a.DefSHA, Mcps: mcps,
		})
	}
	return out, nil
}

func (c *coreImpl) Ledger(q core.LedgerQuery) ([]core.RunRecord, int, error) {
	runs, invalid, err := loadLedger(ledgerPath(c.repoDir))
	if err != nil {
		return nil, 0, err
	}
	out := make([]core.RunRecord, 0, len(runs))
	for _, r := range runs {
		if q.Flow != "" && q.Flow != r.Flow {
			continue
		}
		out = append(out, core.RunRecord{
			Time:          r.Time,
			RunID:         r.RunID,
			Flow:          r.Flow,
			Subject:       r.Subject,
			Revision:      r.Revision,
			ID:            r.ID,
			Branch:        r.Branch,
			PRURL:         r.PRURL,
			Status:        r.Status,
			TokensIn:      r.TokensIn,
			TokensOut:     r.TokOut,
			CacheRead:     r.CacheRead,
			CacheWrite:    r.CacheWrite,
			Reasoning:     r.Reasoning,
			Agent:         r.Agent,
			Model:         r.Model,
			BaseSHA:       r.BaseSHA,
			DefSHA:        r.DefSHA,
			ReviewVerdict: r.ReviewVerdict,
			Error:         r.Error,
		})
	}
	return out, invalid, nil
}

func (c *coreImpl) Trace(runID string) ([]byte, error) {
	runsDir := filepath.Join(c.repoDir, WorkspaceDir, "runs")

	// Verifier and fixer run ids embed the branch ("branch-forest/<branch>"),
	// and filepath.Join in those flows nests the trace below a subdirectory of
	// the runs dir (runs/<timestamp>-branch-forest/<branch>.jsonl). Reproduce
	// the writer's join to reach that nested path, reading the name literally
	// so a glob metacharacter in an item id never matches a sibling. Refuse any
	// id whose joined path escapes the runs dir.
	for _, suffix := range []string{".builder.jsonl", ".verifier.jsonl", ".fixer.jsonl", ".manager.jsonl"} {
		p := filepath.Join(runsDir, filepath.FromSlash(runID)+suffix)
		rel, err := filepath.Rel(runsDir, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		return b, nil
	}
	return nil, fmt.Errorf("trace for run %s not found", runID)
}

// Notes fetches the remote notes, exactly as cmdShow does, then reads the
// verdict and checks notes for one commit.
func (c *coreImpl) Notes(sha string) (core.Verdict, core.Checks, error) {
	if err := fetchNotes(c.repoDir); err != nil {
		return core.Verdict{}, core.Checks{}, &core.StageError{Stage: core.StageFetch, Err: err}
	}
	v, ok, err := readVerdict(c.repoDir, sha)
	if err != nil {
		return core.Verdict{}, core.Checks{}, &core.StageError{Stage: core.StageVerdict, Err: err}
	}
	var verdict core.Verdict
	if ok {
		verdict = core.Verdict{
			Verdict: v.Verdict, Notes: v.Notes, Reviewer: v.Reviewer,
			Model: v.Model, DefSHA: v.DefSHA, RunID: v.RunID, Time: v.Time,
			// A note is only present when it carries a value-a useful decision:
			// matching the surface's old presence heuristic, a note with neither
			// a time nor a run id is not a meaningful entry and must not render.
			Present: v.Time != "" || v.RunID != "",
		}
	}
	ch, ok2, err := readChecks(c.repoDir, sha)
	if err != nil {
		return verdict, core.Checks{}, &core.StageError{Stage: core.StageChecks, Err: err}
	}
	var checks core.Checks
	if ok2 {
		checks = core.Checks{
			Status:  ch.Status,
			RunID:   ch.RunID,
			Time:    ch.Time,
			Present: ch.Time != "" || ch.RunID != "",
		}
		// Always build a non-nil slice so an absent or null results field in the
		// raw note still serializes as an empty array, exactly as the surface's
		// old read rendered it; an explicit empty array keeps its shape too.
		checks.Results = make([]core.CheckResult, 0, len(ch.Results))
		for _, r := range ch.Results {
			checks.Results = append(checks.Results, core.CheckResult{
				Name: r.Name, Code: r.Code, Seconds: r.Seconds, Output: r.Output,
			})
		}
	}
	return verdict, checks, nil
}

// Items returns the backlog the builder selector would act on, including its
// stalled-item filtering, so a surface sees exactly what `forest list` shows.
func (c *coreImpl) Items() ([]core.Item, error) {
	cfg, err := loadConfig(filepath.Join(c.repoDir, "forest.yaml"))
	if err != nil {
		return nil, err
	}
	subjects, err := (builderFlow{}).Select(cfg, c.repoDir)
	if err != nil {
		return nil, err
	}
	return toCoreItems(subjectsToItems(subjects)), nil
}

func subjectsToItems(subjects []Subject) []Item {
	items := make([]Item, 0, len(subjects))
	for _, s := range subjects {
		items = append(items, s.Item)
	}
	return items
}

// EligibleItems returns the tracker backlog without the builder's stalled-item
// filtering: every open item that is not already covered by a forest branch and
// passes the builder's label filters. The live watch board polls this exact
// backlog, which is what it displayed before #176 routed it through the API.
func (c *coreImpl) EligibleItems() ([]core.Item, error) {
	cfg, err := loadConfig(filepath.Join(c.repoDir, "forest.yaml"))
	if err != nil {
		return nil, err
	}
	items, err := eligibleItems(cfg, c.repoDir)
	if err != nil {
		return nil, err
	}
	return toCoreItems(items), nil
}

// toCoreItems adapts controller items into the core package's owned shape.
func toCoreItems(items []Item) []core.Item {
	out := make([]core.Item, 0, len(items))
	for _, it := range items {
		comments := make([]core.Comment, 0, len(it.Comments))
		for _, cm := range it.Comments {
			comments = append(comments, core.Comment{Body: cm.Body, CreatedAt: cm.CreatedAt})
		}
		out = append(out, core.Item{
			ID: it.ID, Title: it.Title, Body: it.Body,
			UpdatedAt: it.UpdatedAt, Tags: it.Tags, Comments: comments,
		})
	}
	return out
}

func (c *coreImpl) Branches() ([]core.BranchState, error) {
	names, err := forestBranches(c.repoDir)
	if err != nil {
		return nil, err
	}
	out := make([]core.BranchState, 0, len(names))
	for _, name := range names {
		head, err := branchHead(c.repoDir, name)
		if err != nil {
			return nil, err
		}
		out = append(out, core.BranchState{Name: name, Head: head})
	}
	return out, nil
}

func (c *coreImpl) Head() (string, error) {
	return gitOut(c.repoDir, "rev-parse", "--short", "HEAD")
}

func (c *coreImpl) Worktrees() ([]string, error) {
	return worktreePaths(c.repoDir), nil
}

func (c *coreImpl) Daemon() (core.Daemon, error) {
	return probeDaemon(c.repoDir), nil
}

// probeDaemon reports whether the factory service is active, preferring
// systemd --user and falling back to the workspace daemon lock.
func probeDaemon(repoDir string) core.Daemon {
	d := core.Daemon{Unit: "forest.service"}
	out, err := exec.Command("systemctl", "--user", "is-active", "forest").Output()
	active := err == nil && strings.TrimSpace(string(out)) == "active"
	d.Active = active
	if active {
		if pid, err := exec.Command("systemctl", "--user", "show", "forest", "-p", "MainPID", "--value").Output(); err == nil {
			d.PID = strings.TrimSpace(string(pid))
		}
		d.Note = "systemd --user"
		return d
	}
	lock := filepath.Join(repoDir, WorkspaceDir, "daemon.lock")
	if f, err := os.Open(lock); err == nil {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		_ = f.Close()
		if err != nil {
			d.Active = true
			d.Note = "daemon.lock held (not via systemd?)"
			return d
		}
	}
	d.Note = "inactive"
	return d
}

// worktreePaths lists every worktree path for the checkout.
func worktreePaths(repoDir string) []string {
	out, err := gitOut(repoDir, "worktree", "list", "--porcelain")
	if err != nil || out == "" {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if path := strings.TrimPrefix(line, "worktree "); path != line && path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}
