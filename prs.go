package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// prState is one lifecycle line for a factory-opened pull request in
// .forest/prs.jsonl. Append-only by construction, like the run ledger.
type prState struct {
	Time   string `json:"time"`
	PRURL  string `json:"pr_url"`
	PR     int    `json:"pr"`
	Branch string `json:"branch"`
	Issue  int    `json:"issue"`
	State  string `json:"state"`  // opened | fixing | ready | merged | closed | stalled
	Owl    string `json:"owl"`    // approve | changes | ""
	Checks string `json:"checks"` // pending | pass | fail | "" (no CI yet)
	SHA    string `json:"sha"`
	Fixes  int    `json:"fixes"`
	Error  string `json:"error,omitempty"`
}

// pulledPR is the shape of a pull request the reaction loop watches.
type pulledPR struct {
	PR             int    `json:"number"`
	URL            string `json:"url"`
	Branch         string `json:"headRefName"`
	State          string `json:"state"`
	MergedAt       string `json:"mergedAt"`
	ReviewDecision string `json:"reviewDecision"`
	HeadSHA        string `json:"oid"`
	CommittedDate  string `json:"committedDate"`
}

func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }

func prFile(workspace string) string { return filepath.Join(workspace, "prs.jsonl") }

func appendPR(workspace string, s prState) error {
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(prFile(workspace), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(s)
}

// lastPRState returns the most recent lifecycle line recorded for a PR.
func lastPRState(workspace string, n int) (prState, bool) {
	b, err := os.ReadFile(prFile(workspace))
	if err != nil {
		return prState{}, false
	}
	var last prState
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s prState
		if json.Unmarshal([]byte(line), &s) != nil {
			continue
		}
		if s.PR == n {
			last, found = s, true
		}
	}
	return last, found
}

// prCurrent records a PR's current state in prs.jsonl, writing nothing when the
// row is unchanged from the PR's most recent recorded state. It is the ledger's
// reconcile oracle: each open PR keeps exactly one current row instead of
// accumulating a stream of duplicates every pass that re-enters the same state.
func prCurrent(workspace string, s prState) error {
	if last, ok := lastPRState(workspace, s.PR); ok && samePRState(last, s) {
		return nil
	}
	return appendPR(workspace, s)
}

// samePRState reports whether two rows describe identical PR state. Time is
// deliberately ignored so a pass that changes nothing does not append a row.
func samePRState(a, b prState) bool {
	return a.PRURL == b.PRURL && a.PR == b.PR && a.Branch == b.Branch &&
		a.Issue == b.Issue && a.State == b.State && a.Owl == b.Owl &&
		a.Checks == b.Checks && a.SHA == b.SHA && a.Fixes == b.Fixes &&
		a.Error == b.Error
}

// fetchOpenPR loads the pull request fields the loop decides on.
func fetchOpenPR(repo string, n int) (pulledPR, error) {
	out, err := ghJSON("pr", "view", fmt.Sprintf("%d", n), "-R", repo,
		"--json", "number,url,headRefName,state,mergedAt,reviewDecision,commits",
		"-q", `{number,url,headRefName,state,mergedAt,reviewDecision,oid:.commits[-1].oid,committedDate:.commits[-1].committedDate}`)
	if err != nil {
		return pulledPR{}, err
	}
	var op pulledPR
	if err := json.Unmarshal(out, &op); err != nil {
		return pulledPR{}, err
	}
	return op, nil
}

// prNumberFromURL extracts the pull request number from its URL.
func prNumberFromURL(u string) int {
	i := strings.LastIndex(u, "/")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(u[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// listOpenForestPRs returns the numbers of open PRs on factory branches.
func listOpenForestPRs(repo string) ([]int, error) {
	out, err := ghJSON("pr", "list", "-R", repo, "--state", "open",
		"--json", "number,headRefName", "--limit", "100")
	if err != nil {
		return nil, err
	}
	var prs []struct {
		Number      int    `json:"number"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	var nums []int
	for _, p := range prs {
		if strings.HasPrefix(p.HeadRefName, "forest/") {
			nums = append(nums, p.Number)
		}
	}
	return nums, nil
}

// checkRollup summarizes commit check-runs as pending | pass | fail | "".
// "" means there is no CI check yet, so nothing may merge on third-party
// review bots alone (CodeRabbit, Bugbot).
func checkRollup(repo, sha string) (string, error) {
	out, err := ghJSON("api", fmt.Sprintf("repos/%s/commits/%s/check-runs", repo, sha),
		"-q", `[.check_runs[] | {status, conclusion, name}]`)
	if err != nil {
		return "", err
	}
	var runs []checkRun
	if err := json.Unmarshal(out, &runs); err != nil {
		if len(bytes.TrimSpace(out)) == 0 {
			return "", nil
		}
		return "", err
	}
	if len(runs) == 0 {
		return "", nil
	}
	return rollupChecks(runs), nil
}

// checkRun is one check-run as returned by the commits/check-runs endpoint.
type checkRun struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Name       string `json:"name"`
}

// rollupChecks maps check-runs to pending | pass | fail | "". A run that never
// concluded — still running, or completed with an empty conclusion — counts as
// pending: it must never unlock a merge as a pass.
func rollupChecks(runs []checkRun) string {
	haveCI, anyPending, allPass := false, false, true
	for _, r := range runs {
		l := strings.ToLower(r.Name)
		if strings.Contains(l, "ci") || strings.Contains(l, "test") {
			haveCI = true
		}
		conclusion := strings.ToLower(r.Conclusion)
		// The REST API returns lowercase status/conclusion values.
		if strings.ToLower(r.Status) != "completed" || conclusion == "" {
			anyPending = true
			continue
		}
		switch conclusion {
		case "success", "neutral", "skipped":
		default:
			allPass = false
		}
	}
	if !allPass {
		return "fail"
	}
	if anyPending {
		return "pending"
	}
	if !haveCI {
		return ""
	}
	return "pass"
}

// latestRequestedChange returns the newest CHANGES_REQUESTED review and the
// commit it was submitted against, via the REST reviews endpoint.
func latestRequestedChange(repo string, n int) (body, commitID string, err error) {
	out, err := ghJSON("api", fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, n), "-q",
		`[.[] | select(.state == "CHANGES_REQUESTED")][-1] | {body, commit_id}`)
	if err != nil || len(bytes.TrimSpace(out)) == 0 || bytes.TrimSpace(out)[0] == 'n' {
		return "", "", err
	}
	var r struct {
		Body     string `json:"body"`
		CommitID string `json:"commit_id"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", "", err
	}
	return r.Body, r.CommitID, nil
}

// failingChecks lists names+conclusions of failed checks on a commit.
func failingChecks(repo, sha string) (string, error) {
	out, err := ghJSON("api", fmt.Sprintf("repos/%s/commits/%s/check-runs", repo, sha),
		"-q", `[.check_runs[] | select(.status == "completed" and .conclusion != "success" and .conclusion != "neutral" and .conclusion != "skipped") | .name + ": " + (.conclusion // "unknown")]`)
	if err != nil {
		return "", err
	}
	var lines []string
	if len(bytes.TrimSpace(out)) == 0 {
		return "", nil
	}
	if err := json.Unmarshal(out, &lines); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

func mergePR(repo string, n int) error {
	_, err := ghJSON("pr", "merge", fmt.Sprintf("%d", n), "-R", repo, "--squash", "--delete-branch")
	return err
}

// watchPR drives one open factory PR: re-enter on a change request or CI
// failure, merge when owl approved + CI green + auto_merge on. Every state
// change is appended to prs.jsonl. Returns 0 on handled outcomes.
func watchPR(cfg Config, repoDir string, n int) int {
	workspace := filepath.Join(repoDir, WorkspaceDir)
	op, err := fetchOpenPR(cfg.Repo, n)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: pr %d: %v\n", n, err)
		return 1
	}
	last, known := lastPRState(workspace, n)
	base := prState{Time: nowRFC(), PR: op.PR, PRURL: op.URL, Branch: op.Branch}
	sig := func(s prState) string {
		return fmt.Sprintf("%s|%s|%s|%d|%s|%s", s.State, s.Checks, s.SHA, s.Fixes, s.Owl, s.Error)
	}
	// Skip the append burst: a PR that did not change is recorded once.
	unchanged := func() bool {
		return known && sig(base) == sig(last)
	}

	if op.State != "OPEN" {
		base.State = "closed"
		_ = prCurrent(workspace, base)
		return 0
	}
	if !known {
		// Not opened by this factory session: record idly, never act on it.
		base.State = "opened"
		_ = prCurrent(workspace, base)
		return 0
	}
	if last.State == "merged" {
		return 0
	}
	base.Issue = last.Issue
	base.SHA = op.HeadSHA
	base.Fixes = last.Fixes
	base.Owl = last.Owl

	// A CHANGES_REQUESTED is actionable only when it targets the current head:
	// a review on code we already fixed is stale and must not re-enter.
	blocked := false
	if op.ReviewDecision == "CHANGES_REQUESTED" {
		_, commitID, rerr := latestRequestedChange(cfg.Repo, n)
		if rerr == nil {
			blocked = commitID == op.HeadSHA
		}
	}
	checks := ""
	if c, cerr := checkRollup(cfg.Repo, op.HeadSHA); cerr == nil {
		checks = c
	}
	base.Checks = checks

	if blocked {
		feedback, _, _ := latestRequestedChange(cfg.Repo, n)
		return fixPR(cfg, repoDir, op, base, feedback)
	}
	if checks == "fail" {
		note, _ := failingChecks(cfg.Repo, op.HeadSHA)
		return fixPR(cfg, repoDir, op, base, "CI failed:\n"+note)
	}
	if last.Owl == "approve" && checks == "pass" {
		if cfg.Workflow.AutoMerge {
			if err := mergePR(cfg.Repo, n); err != nil {
				base.State = "stalled"
				base.Error = err.Error()
				_ = prCurrent(workspace, base)
				fmt.Fprintf(os.Stderr, "forest: merge #%d: %v\n", n, err)
				return 1
			}
			base.State = "merged"
			_ = prCurrent(workspace, base)
			fmt.Printf("forest: pr #%d merged\n", n)
			return 0
		}
		base.State = "ready"
		if !unchanged() {
			_ = prCurrent(workspace, base)
		}
		fmt.Printf("forest: pr #%d ready to merge (auto_merge off): %s\n", n, op.URL)
		return 0
	}
	base.State = "opened"
	if !unchanged() {
		_ = prCurrent(workspace, base)
	}
	return 0
}

// fixLimitReached reports whether a PR's recorded fix count already consumed
// the reaction-fix budget configured on cfg. The loop asks this before each
// re-entry; once true it stops attempting fixes and parks the PR instead.
func fixLimitReached(cfg Config, prior prState) bool {
	return prior.Fixes >= cfg.Workflow.MaxReactionFixes
}

// recordFixFailure persists one failed fix attempt on a PR: it increments the
// recorded fix count, writes a PR state row, and marks the PR stalled once the
// configured reaction-fix budget is spent. It returns the appended row and
// whether the PR was parked. A failed fix must count against the budget or a
// stuck PR is retried forever.
func recordFixFailure(cfg Config, workspace string, prior prState, errMsg string) (prState, bool) {
	prior.Fixes++
	prior.Error = errMsg
	prior.Time = nowRFC()
	stalled := fixLimitReached(cfg, prior)
	if stalled {
		prior.State = "stalled"
	} else {
		prior.State = "fixing"
	}
	_ = prCurrent(workspace, prior)
	return prior, stalled
}

// fixPR re-enters a PR's item at the build phase on its existing branch: the
// owl re-verifies and forest pushes a new commit. It never re-enters past the
// reaction-fix budget configured on cfg and parks the PR once that budget is
// spent.
func fixPR(cfg Config, repoDir string, op pulledPR, prior prState, feedback string) int {
	workspace := filepath.Join(repoDir, WorkspaceDir)
	if fixLimitReached(cfg, prior) {
		prior.State = "stalled"
		prior.Error = "too many fix cycles"
		prior.Time = nowRFC()
		_ = prCurrent(workspace, prior)
		fmt.Fprintf(os.Stderr, "forest: pr #%d stalled after %d fixes\n", op.PR, prior.Fixes)
		return 1
	}
	it, err := getIssueAny(cfg.Repo, prior.Issue)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: pr #%d issue %d: %v\n", op.PR, prior.Issue, err)
		return 1
	}
	runID := fmt.Sprintf("%s-%d-fix%d", time.Now().UTC().Format("20060102T150405Z"), prior.Issue, prior.Fixes+1)
	wtDir, baseSHA, err := createWorktreeAtBranch(repoDir, workspace, op.Branch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: pr #%d worktree: %v\n", op.PR, err)
		return 1
	}
	// createWorktreeAtBranch registered wtDir in the tracker so the re-enter's
	// worktree is also covered by the second-signal handler; clear it here.
	defer func() {
		removeWorktree(repoDir, wtDir)
		setCurrentWorktree("")
	}()

	id := identify(repoDir, cfg)
	r, err := runPick(cfg, repoDir, wtDir, baseSHA, runID, it, feedback)
	if err != nil {
		rec := runRecord{
			Time: nowRFC(), RunID: runID, Issue: it.Number, Branch: op.Branch,
			PRURL: op.URL, Status: failStatus(err), Error: err.Error(),
			Agent: id.Agents, Model: id.Models, DefSHA: id.DefSHA,
		}
		_ = appendRun(workspace, rec)
		prior, stalled := recordFixFailure(cfg, workspace, prior, err.Error())
		if stalled {
			fmt.Fprintf(os.Stderr, "forest: pr #%d stalled after %d fixes: %v\n", op.PR, prior.Fixes, err)
		} else {
			fmt.Fprintf(os.Stderr, "forest: pr #%d fix %d: %v\n", op.PR, prior.Fixes, err)
		}
		return 1
	}
	status := "fixed"
	verdict := r.Verdict
	if verdict != "approve" {
		status = "review_failed"
	}
	rec := runRecord{
		Time: nowRFC(), RunID: runID, Issue: it.Number, Branch: op.Branch, PRURL: op.URL,
		Status: status, CostUSD: r.Cost, TokensIn: r.TokIn, TokOut: r.TokOut,
		Agent: id.Agents, Model: id.Models,
		BaseSHA: baseSHA, DefSHA: id.DefSHA, ReviewVerdict: verdict,
	}
	_ = appendRun(workspace, rec)
	if verdict != "approve" {
		prior.State = "stalled"
		prior.Error = cfg.Workflow.Review + " did not approve the fix"
		prior.Fixes++
		prior.Owl = r.Verdict
		prior.Time = nowRFC()
		_ = prCurrent(workspace, prior)
		fmt.Fprintf(os.Stderr, "forest: pr #%d fix not approved by owl\n", op.PR)
		return 1
	}
	if err := commitAndPush(repoDir, wtDir, op.Branch, it); err != nil {
		prior.State = "stalled"
		prior.Error = err.Error()
		prior.Time = nowRFC()
		_ = prCurrent(workspace, prior)
		fmt.Fprintf(os.Stderr, "forest: pr #%d push: %v\n", op.PR, err)
		return 1
	}
	newSHA, _ := gitOut(wtDir, "rev-parse", "HEAD")
	prior.State = "fixing"
	prior.Fixes++
	prior.Owl = r.Verdict
	prior.SHA = newSHA
	prior.Checks = "pending"
	prior.Error = ""
	prior.Time = nowRFC()
	_ = prCurrent(workspace, prior)
	fmt.Printf("forest: pr #%d fix pushed %s\n", op.PR, short(newSHA))
	return 0
}
