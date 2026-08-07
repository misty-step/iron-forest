package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// projectionCommand is the host boundary. Projection effects remain optional,
// and tests replace this function without invoking the host CLI.
var projectionCommand = ghJSON

type projectionPullRequest struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	HeadSHA string `json:"headRefOid"`
}

func openProjectionPR(cfg Config, branch string) ([]projectionPullRequest, error) {
	out, err := projectionCommand("pr", "list", "-R", cfg.Repo, "--state", "open",
		"--head", branch, "--json", "number,url,headRefOid")
	if err != nil {
		return nil, err
	}
	var prs []projectionPullRequest
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	return prs, nil
}

// projectBranch publishes a built branch as one idempotent pull request.
func projectBranch(cfg Config, it Item, branch, body string) (string, error) {
	if !cfg.Projection.Enabled {
		return "", nil
	}
	prs, err := openProjectionPR(cfg, branch)
	if err != nil {
		return "", err
	}
	if len(prs) > 0 {
		return prs[0].URL, nil
	}
	created, err := projectionCommand("pr", "create", "-R", cfg.Repo,
		"--base", "master", "--head", branch,
		"--title", "forest: "+it.Title, "--body", body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(created)), nil
}

func verdictBody(v verdictNote, c checksNote) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Verdict: %s\n", v.Verdict)
	fmt.Fprintf(&b, "Notes: %s\n\n", v.Notes)
	fmt.Fprintf(&b, "Checks: %s\n", c.Status)
	for _, result := range c.Results {
		fmt.Fprintf(&b, "- %s: code=%d seconds=%.3f\n", result.Name, result.Code, result.Seconds)
		if result.Output != "" {
			fmt.Fprintf(&b, "  %s\n", result.Output)
		}
	}
	return b.String()
}

// projectVerdict mirrors a git verdict and its checks as one pull-request comment.
func projectVerdict(cfg Config, branch string, v verdictNote, c checksNote) error {
	if !cfg.Projection.Enabled {
		return nil
	}
	prs, err := openProjectionPR(cfg, branch)
	if err != nil {
		return err
	}
	if len(prs) == 0 {
		return nil
	}
	_, err = projectionCommand("pr", "comment", strconv.Itoa(prs[0].Number),
		"-R", cfg.Repo, "--body", verdictBody(v, c))
	return err
}

// projectChecks mirrors a failing check result on the human surface. The
// Verifier stops before review when a check fails, so this is the only signal
// an operator would otherwise get for that head.
func projectChecks(cfg Config, branch string, c checksNote) error {
	if !cfg.Projection.Enabled {
		return nil
	}
	prs, err := openProjectionPR(cfg, branch)
	if err != nil {
		return err
	}
	if len(prs) == 0 {
		return nil
	}
	_, err = projectionCommand("pr", "comment", strconv.Itoa(prs[0].Number),
		"-R", cfg.Repo, "--body", checksSummary(c)+"\n\n"+verdictBody(verdictNote{Verdict: "pending"}, c))
	return err
}

// projectMerge asks the host to merge a pull request when it owns the target
// branch. expectedHead is the exact revision the git side already admitted, and
// the host merge is only legal when the projection still points at it: a push to
// the branch after admission would otherwise make the host land an unchecked,
// unreviewed commit that no local checks or Verdict describe. The PR's head must
// therefore be reported and non-empty, and match expectedHead: an empty head is
// not an admission the machine can trust, so it is refused rather than let past.
// The merge call then carries the provider's own compare-and-swap
// (--expected-head), so even if a push lands between the list and the merge, the
// host refuses rather than landing a head no local Checks or Verdict describe.
func projectMerge(cfg Config, branch, strategy, expectedHead string) error {
	if !cfg.Projection.Enabled {
		return errors.New("projection disabled")
	}
	prs, err := openProjectionPR(cfg, branch)
	if err != nil {
		return err
	}
	if len(prs) == 0 || prs[0].Number == 0 {
		return fmt.Errorf("no open pull request for branch %q", branch)
	}
	if prs[0].HeadSHA == "" || prs[0].HeadSHA != expectedHead {
		return fmt.Errorf("projection head %q does not match the admitted revision %s for branch %q; refusing",
			short(prs[0].HeadSHA), short(expectedHead), branch)
	}
	method := ""
	switch strategy {
	case "squash":
		method = "--squash"
	case "ff":
		// --rebase replays the branch's own commits. --merge would add a merge
		// commit, which contradicts the declared fast-forward shape.
		method = "--rebase"
	default:
		return fmt.Errorf("unsupported merge strategy %q", strategy)
	}
	_, err = projectionCommand("pr", "merge", strconv.Itoa(prs[0].Number),
		"-R", cfg.Repo, method, "--expected-head", prs[0].HeadSHA)
	return err
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
