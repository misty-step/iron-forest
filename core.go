package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// AgentInfo summarizes one declared agent for a surface. It is the read view a
// surface needs to list agents without touching the declaration files.
type AgentInfo struct {
	Name        string
	Description string
	Model       string
	Variant     string
	Mode        string
	DefSHA      string
	Mcps        []McpSpec
}

// RunRecord is one append-only ledger row in the shape a surface reads. It
// mirrors runRecord so a surface never depends on the controller's rows.
type RunRecord struct {
	Time          string
	RunID         string
	Flow          string
	Subject       string
	Revision      string
	ID            string
	Branch        string
	PRURL         string
	Status        string
	TokensIn      int64
	TokensOut     int64
	Agent         string
	Model         string
	BaseSHA       string
	DefSHA        string
	ReviewVerdict string
	Error         string
}

// LedgerQuery filters the read of the run ledger. A zero value reads every row,
// which is exactly what the stats command aggregates today.
type LedgerQuery struct {
	Flow string
}

// Verdict is the review decision note for one commit.
type Verdict struct {
	Verdict  string
	Notes    string
	Reviewer string
	Model    string
	DefSHA   string
	RunID    string
	Time     string
}

// CheckResult is one gate command and its observed result.
type CheckResult struct {
	Name    string
	Code    int
	Seconds float64
	Output  string
}

// Checks is the gate results note for one commit.
type Checks struct {
	Status  string
	Results []CheckResult
	RunID   string
	Time    string
}

// BranchState is one forest branch and its head commit.
type BranchState struct {
	Name string
	Head string
}

// API is every durable operation a surface may perform. Live-run operations
// are deliberately absent: they need the daemon (see #163).
type API interface {
	Config() (Config, error)
	Agents() ([]AgentInfo, error)
	Ledger(LedgerQuery) ([]RunRecord, error)
	Trace(runID string) ([]byte, error)
	Notes(sha string) (Verdict, Checks, error)
	Items() ([]Item, error)
	Branches() ([]BranchState, error)
	DaemonPresent() (bool, error)
}

// core forwards every durable operation to the existing package functions. It
// is a forwarding layer on purpose: this card defines the seam, not the move.
type core struct {
	repoDir string
}

// NewCore returns the API for a checkout at repoDir, usable without a daemon.
func NewCore(repoDir string) API {
	return &core{repoDir: repoDir}
}

func (c *core) Config() (Config, error) {
	return loadConfig(filepath.Join(c.repoDir, "forest.yaml"))
}

func (c *core) Agents() ([]AgentInfo, error) {
	names, err := discoverAgents(c.repoDir)
	if err != nil {
		return nil, err
	}
	out := make([]AgentInfo, 0, len(names))
	for _, name := range names {
		a, err := loadAgent(c.repoDir, name)
		if err != nil {
			return nil, err
		}
		out = append(out, AgentInfo{
			Name:        a.Name,
			Description: a.Description,
			Model:       a.Model,
			Variant:     a.Variant,
			Mode:        a.Mode,
			DefSHA:      a.DefSHA,
			Mcps:        a.MCP,
		})
	}
	return out, nil
}

func (c *core) Ledger(q LedgerQuery) ([]RunRecord, error) {
	runs, _, err := loadLedger(ledgerPath(c.repoDir))
	if err != nil {
		return nil, err
	}
	out := make([]RunRecord, 0, len(runs))
	for _, r := range runs {
		if q.Flow != "" && q.Flow != r.Flow {
			continue
		}
		out = append(out, RunRecord{
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
			Agent:         r.Agent,
			Model:         r.Model,
			BaseSHA:       r.BaseSHA,
			DefSHA:        r.DefSHA,
			ReviewVerdict: r.ReviewVerdict,
			Error:         r.Error,
		})
	}
	return out, nil
}

func (c *core) Trace(runID string) ([]byte, error) {
	runsDir := filepath.Join(c.repoDir, WorkspaceDir, "runs")
	matches, err := filepath.Glob(filepath.Join(runsDir, runID+".*.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("trace for run %s not found", runID)
	}
	return os.ReadFile(matches[0])
}

func (c *core) Notes(sha string) (Verdict, Checks, error) {
	v, ok, err := readVerdict(c.repoDir, sha)
	if err != nil {
		return Verdict{}, Checks{}, err
	}
	var verdict Verdict
	if ok {
		verdict = Verdict{
			Verdict:  v.Verdict,
			Notes:    v.Notes,
			Reviewer: v.Reviewer,
			Model:    v.Model,
			DefSHA:   v.DefSHA,
			RunID:    v.RunID,
			Time:     v.Time,
		}
	}
	ch, ok2, err := readChecks(c.repoDir, sha)
	if err != nil {
		return verdict, Checks{}, err
	}
	var checks Checks
	if ok2 {
		checks = Checks{
			Status:  ch.Status,
			Results: make([]CheckResult, 0, len(ch.Results)),
			RunID:   ch.RunID,
			Time:    ch.Time,
		}
		for _, r := range ch.Results {
			checks.Results = append(checks.Results, CheckResult{
				Name: r.Name, Code: r.Code, Seconds: r.Seconds, Output: r.Output,
			})
		}
	}
	return verdict, checks, nil
}

func (c *core) Items() ([]Item, error) {
	cfg, err := loadConfig(filepath.Join(c.repoDir, "forest.yaml"))
	if err != nil {
		return nil, err
	}
	return eligibleItems(cfg, c.repoDir)
}

func (c *core) Branches() ([]BranchState, error) {
	names, err := forestBranches(c.repoDir)
	if err != nil {
		return nil, err
	}
	out := make([]BranchState, 0, len(names))
	for _, name := range names {
		head, err := branchHead(c.repoDir, name)
		if err != nil {
			return nil, err
		}
		out = append(out, BranchState{Name: name, Head: head})
	}
	return out, nil
}

func (c *core) DaemonPresent() (bool, error) {
	s := probeDaemon(c.repoDir)
	return s.Active, nil
}
