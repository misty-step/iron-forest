package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// orphan is one factory pull request whose work item closed without the
// change landing: its originating issue is closed yet the PR never merged.
type orphan struct {
	PR     int    `json:"pr"`
	Branch string `json:"branch"`
	Issue  int    `json:"issue"`
	Reason string `json:"reason"`
}

// latestPRStates returns the most recent prState recorded per pull request in
// .forest/prs.jsonl. Unparseable lines and rows without a PR number are
// skipped so one bad artifact never hides the rest of the ledger.
func latestPRStates(workspace string) ([]prState, error) {
	b, err := os.ReadFile(filepath.Join(workspace, "prs.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	last := map[int]prState{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s prState
		if json.Unmarshal([]byte(line), &s) != nil || s.PR == 0 {
			continue
		}
		last[s.PR] = s
	}
	out := make([]prState, 0, len(last))
	for _, s := range last {
		out = append(out, s)
	}
	return out, nil
}

// orphanReason explains, for one unlanded PR row, why it is orphaned. The
// message names both the PR and the closed issue that no longer carries it.
func orphanReason(s prState) string {
	switch s.State {
	case "closed":
		return fmt.Sprintf("issue #%d closed; PR #%d closed without merging", s.Issue, s.PR)
	case "stalled":
		return fmt.Sprintf("issue #%d closed; PR #%d stalled and unmerged", s.Issue, s.PR)
	default:
		return fmt.Sprintf("issue #%d closed but PR #%d still open", s.Issue, s.PR)
	}
}

// detectOrphans is the reconcile pass. It returns every pull request whose
// originating issue is closed but whose change never landed (the PR is not
// merged). liveMerged reports a PR's current merge state and issueClosed an
// issue's current state; injecting them keeps the pass offline-testable. A
// merged PR is never orphaned — the live merge check is authoritative, so a PR
// merged after its last recorded opened/ready/stalled row is still cleared —
// and a row without an issue attribution cannot be matched and is skipped.
func detectOrphans(workspace string, liveMerged func(int) (bool, error), issueClosed func(int) (bool, error)) ([]orphan, error) {
	prs, err := latestPRStates(workspace)
	if err != nil {
		return nil, err
	}
	var out []orphan
	for _, s := range prs {
		if s.State == "merged" || s.Issue == 0 {
			continue
		}
		merged, err := liveMerged(s.PR)
		if err != nil {
			return nil, fmt.Errorf("pr %d: %w", s.PR, err)
		}
		if merged {
			continue
		}
		closed, err := issueClosed(s.Issue)
		if err != nil {
			return nil, fmt.Errorf("issue %d: %w", s.Issue, err)
		}
		if !closed {
			continue
		}
		out = append(out, orphan{PR: s.PR, Branch: s.Branch, Issue: s.Issue, Reason: orphanReason(s)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PR < out[j].PR })
	return out, nil
}

// prIsMerged reports whether a GitHub pull request is currently merged. The
// live merge state is authoritative: reconcile consults it so a PR merged after
// its last opened/ready/stalled ledger row is never reported orphaned.
func prIsMerged(repo string, n int) (bool, error) {
	out, err := ghJSON("pr", "view", fmt.Sprintf("%d", n), "-R", repo,
		"--json", "state", "-q", ".state")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "MERGED", nil
}

// issueIsClosed reports whether a GitHub issue is currently closed.
func issueIsClosed(repo string, n int) (bool, error) {
	out, err := ghJSON("issue", "view", fmt.Sprintf("%d", n), "-R", repo,
		"--json", "state", "-q", ".state")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "CLOSED", nil
}

// cmdReconcile runs `forest reconcile`: it reports every orphaned pull request
// — one whose issue closed without the change landing — with its originating
// issue id and the reason it is orphaned, so the work is surfaced, not
// forgotten. It exits 0 whether or not orphans exist, and 1 only on error.
func cmdReconcile(cfg Config, repoDir string) int {
	workspace := filepath.Join(repoDir, WorkspaceDir)
	got, err := detectOrphans(workspace,
		func(n int) (bool, error) { return prIsMerged(cfg.Repo, n) },
		func(n int) (bool, error) { return issueIsClosed(cfg.Repo, n) })
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: reconcile: %v\n", err)
		return 1
	}
	if len(got) == 0 {
		fmt.Println("forest: no orphaned pull requests")
		return 0
	}
	for _, o := range got {
		fmt.Printf("forest: orphan PR #%-4d issue #%-4d branch=%-24s %s\n",
			o.PR, o.Issue, o.Branch, o.Reason)
	}
	return 0
}
