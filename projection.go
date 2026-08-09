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

var errHostMergePending = errors.New("Host merge is pending")
var errHostMergeUnavailable = errors.New("Host merge request is unavailable")
var errHostMergeNoView = errors.New("Host merge request has no visible view")

type projectionPullRequest struct {
	Number            int    `json:"number"`
	URL               string `json:"url"`
	HeadRefOID        string `json:"headRefOid"`
	HeadRefName       string `json:"headRefName"`
	BaseRefName       string `json:"baseRefName"`
	IsCrossRepository bool   `json:"isCrossRepository"`
}

func projectionPRs(cfg Config, branch string) ([]projectionPullRequest, error) {
	out, err := projectionCommand("pr", "list", "-R", cfg.Repo, "--state", "open",
		"--head", branch, "--json", "number,url,headRefOid,headRefName,baseRefName,isCrossRepository")
	if err != nil {
		return nil, err
	}
	var prs []projectionPullRequest
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	for _, pr := range prs {
		if pr.IsCrossRepository || pr.HeadRefName != branch || pr.BaseRefName != "master" {
			return nil, fmt.Errorf("pull request %d does not originate from %s branch %q and target master",
				pr.Number, cfg.Repo, branch)
		}
	}
	return prs, nil
}

type projectionRESTPullRequest struct {
	Number   int     `json:"number"`
	URL      string  `json:"html_url"`
	MergedAt *string `json:"merged_at"`
	Head     struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func mergedProjectionPRs(cfg Config, branch string) ([]projectionPullRequest, error) {
	owner := strings.SplitN(cfg.Repo, "/", 2)[0]
	out, err := projectionCommand("api", "--method", "GET", "--paginate", "--slurp",
		"repos/"+cfg.Repo+"/pulls", "--field", "state=closed",
		"--field", "head="+owner+":"+branch, "--field", "base=master", "--field", "per_page=100")
	if err != nil {
		return nil, err
	}
	var pages [][]projectionRESTPullRequest
	if err := json.Unmarshal(out, &pages); err != nil {
		return nil, err
	}
	var prs []projectionPullRequest
	for _, page := range pages {
		for _, raw := range page {
			pr := projectionPullRequest{
				Number:            raw.Number,
				URL:               raw.URL,
				HeadRefOID:        raw.Head.SHA,
				HeadRefName:       raw.Head.Ref,
				BaseRefName:       raw.Base.Ref,
				IsCrossRepository: raw.Head.Repo == nil || raw.Head.Repo.FullName != cfg.Repo,
			}
			if pr.IsCrossRepository || pr.HeadRefName != branch || pr.BaseRefName != "master" {
				return nil, fmt.Errorf("pull request %d does not originate from %s branch %q and target master",
					pr.Number, cfg.Repo, branch)
			}
			if raw.MergedAt == nil || *raw.MergedAt == "" {
				continue
			}
			prs = append(prs, pr)
		}
	}
	return prs, nil
}

func openProjectionPR(cfg Config, branch string) ([]projectionPullRequest, error) {
	prs, err := projectionPRs(cfg, branch)
	if err != nil {
		return nil, err
	}
	if len(prs) > 1 {
		return nil, fmt.Errorf("multiple open pull requests for branch %q", branch)
	}
	return prs, nil
}

// projectBranch publishes a built branch as one idempotent pull request.
// expectedHead is empty for initial Builder publication and set for Verifier
// publication or recovery. It fences open and merged requests to that Revision.
func projectBranch(cfg Config, it Item, branch, body, expectedHead string) (string, bool, error) {
	if !cfg.Projection.Enabled {
		return "", false, nil
	}
	prs, err := openProjectionPR(cfg, branch)
	if err != nil {
		return "", false, err
	}
	if len(prs) > 0 {
		if expectedHead != "" && prs[0].HeadRefOID != "" && prs[0].HeadRefOID != expectedHead {
			return "", false, fmt.Errorf("%w: open pull request for branch %q moved to %s after reviewed Revision %s", errHostMergeUnavailable, branch, prs[0].HeadRefOID, expectedHead)
		}
		return prs[0].URL, false, nil
	}
	if expectedHead != "" {
		merged, err := mergedProjectionPRs(cfg, branch)
		if err != nil {
			return "", false, err
		}
		for _, pr := range merged {
			if pr.HeadRefOID == expectedHead {
				return pr.URL, cfg.Projection.MergeViaHost, nil
			}
		}
		if len(merged) > 0 {
			return "", false, fmt.Errorf("%w: merged pull request for branch %q does not match reviewed Revision %s", errHostMergeUnavailable, branch, expectedHead)
		}
	}
	created, err := projectionCommand("pr", "create", "-R", cfg.Repo,
		"--base", "master", "--head", branch,
		"--title", redactSecretShaped("forest: "+it.Title),
		"--body", redactSecretShaped(body))
	if err != nil {
		return "", false, err
	}
	return strings.TrimSpace(string(created)), false, nil
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

// Projection comment bodies cross the Host boundary through one redacted sink.
func projectComment(cfg Config, branch, body string) error {
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
		"-R", cfg.Repo, "--body", redactSecretShaped(body))
	return err
}

// projectVerdict mirrors a git verdict and its checks as one pull-request comment.
func projectVerdict(cfg Config, branch string, v verdictNote, c checksNote) error {
	return projectComment(cfg, branch, verdictBody(v, c))
}

// projectChecks mirrors a failing check result on the human surface. The
// Verifier stops before review when a check fails, so this is the only signal
// an operator would otherwise get for that head.
func projectChecks(cfg Config, branch string, c checksNote) error {
	return projectComment(cfg, branch, checksSummary(c)+"\n\n"+verdictBody(verdictNote{Verdict: "pending"}, c))
}

// projectMerge asks the host to merge a pull request when it owns the target
// branch. expectedHead is the reviewed Revision the merge must still point at.
// A merged request with that exact head is durable recovery evidence: a prior
// host merge succeeded, so the caller can retry Tracker retirement without
// asking an already-closed request to merge again.
func projectMerge(cfg Config, branch, strategy, expectedHead string) error {
	merged, pr, err := inspectProjectMerge(cfg, branch, strategy, expectedHead)
	if err != nil {
		if errors.Is(err, errHostMergeNoView) {
			return errHostMergePending
		}
		return err
	}
	if merged {
		return nil
	}
	args := []string{"pr", "merge", strconv.Itoa(pr.Number),
		"-R", cfg.Repo, "--squash"}
	if expectedHead != "" {
		args = append(args, "--match-head-commit", expectedHead)
	}
	if _, err := projectionCommand(args...); err != nil {
		// The command can fail after the Host accepted or queued the request.
		// Preserve intent and determine the exact merged head on a later pass.
		return fmt.Errorf("%w: merge request: %v", errHostMergePending, err)
	}
	merged, _, err = inspectProjectMerge(cfg, branch, strategy, expectedHead)
	if err != nil {
		if errors.Is(err, errHostMergeUnavailable) {
			return err
		}
		// The Host accepted the command. A merge queue can briefly expose
		// neither view, so keep durable intent and observe it on the next pass.
		return fmt.Errorf("%w: post-request state: %v", errHostMergePending, err)
	}
	if !merged {
		return errHostMergePending
	}
	return nil
}

func inspectProjectMerge(cfg Config, branch, strategy, expectedHead string) (bool, projectionPullRequest, error) {
	if !cfg.Projection.Enabled {
		return false, projectionPullRequest{}, errors.New("projection disabled")
	}
	if strategy != "squash" {
		return false, projectionPullRequest{}, fmt.Errorf("Host projection supports only squash merge, got %q", strategy)
	}
	merged, err := mergedProjectionPRs(cfg, branch)
	if err != nil {
		return false, projectionPullRequest{}, err
	}
	for _, pr := range merged {
		if expectedHead == "" || pr.HeadRefOID == expectedHead {
			return true, pr, nil
		}
	}
	prs, err := openProjectionPR(cfg, branch)
	if err != nil {
		return false, projectionPullRequest{}, err
	}
	if len(prs) == 0 {
		if len(merged) > 0 {
			return false, projectionPullRequest{}, fmt.Errorf("%w: merged pull request for branch %q does not match reviewed Revision %s",
				errHostMergeUnavailable, branch, expectedHead)
		}
		return false, projectionPullRequest{}, fmt.Errorf("%w: no open or merged pull request for branch %q",
			errHostMergeNoView, branch)
	}
	pr := prs[0]
	if pr.Number == 0 {
		return false, projectionPullRequest{}, fmt.Errorf("%w: open pull request for branch %q has no number",
			errHostMergeUnavailable, branch)
	}
	if expectedHead != "" && pr.HeadRefOID != "" && pr.HeadRefOID != expectedHead {
		return false, projectionPullRequest{}, fmt.Errorf("%w: open pull request for branch %q moved to %s after reviewed Revision %s",
			errHostMergeUnavailable, branch, pr.HeadRefOID, expectedHead)
	}
	return false, pr, nil
}
