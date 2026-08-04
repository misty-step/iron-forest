package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// report is the typed envelope the agent must write before it stops. It is
// the only channel that crosses the seam between the agent and the factory.
type report struct {
	Summary      string   `json:"summary"`
	ChangedFiles []string `json:"changed_files"`
	Notes        string   `json:"notes"`
}

// gate verifies the agent's claims against reality after the run:
//   - the agent did not commit (HEAD is still the base)
//   - it produced a non-empty change
//   - it did not touch a protected path
//   - report.json exists and parses
//
// It returns the changed file list for the pull request body.
func gate(wtDir, baseSHA string, protected []string) ([]string, report, error) {
	var rep report
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, rep, fmt.Errorf("cannot read worktree HEAD: %w", err)
	}
	if head != baseSHA {
		return nil, rep, fmt.Errorf("agent committed: HEAD moved %s -> %s", short(baseSHA), short(head))
	}
	out, err := gitOut(wtDir, "status", "--porcelain")
	if err != nil {
		return nil, rep, err
	}
	changed := parseChanged(out)
	if len(changed) == 0 {
		return nil, rep, fmt.Errorf("agent produced no changes")
	}
	for _, path := range changed {
		for _, prot := range protected {
			if prot != "" && (path == prot || strings.HasPrefix(path, prot)) {
				return nil, rep, fmt.Errorf("agent touched protected path %q", path)
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(wtDir, "report.json"))
	if err != nil {
		return nil, rep, fmt.Errorf("report.json missing: %w", err)
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, rep, fmt.Errorf("report.json is invalid JSON: %w", err)
	}
	if strings.TrimSpace(rep.Summary) == "" {
		return nil, rep, fmt.Errorf("report.json has an empty summary")
	}
	return changed, rep, nil
}

// parseChanged turns `git status --porcelain` into a changed file list,
// stripping rename arrows (`R  old -> new` keeps `new`).
func parseChanged(porcelain string) []string {
	var out []string
	for _, line := range strings.Split(porcelain, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		out = append(out, path)
	}
	return out
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
