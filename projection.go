package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// projectionCommand is the host boundary. Projection effects remain optional,
// and tests replace this function without invoking the host CLI.
var projectionCommand = ghJSON

func warnEffect(effect string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: %s warning: %s\n", effect, redactSecretShaped(err.Error()))
	}
}

func warnProjection(err error) {
	warnEffect("Projection", err)
}

var errHostMergePending = errors.New("Host merge is pending")

var errHostMergeRequestFailed = errors.New("Host merge request failed")
var errHostMergeAccepted = errors.New("Host merge request was accepted")
var errHostMergeUnavailable = errors.New("Host merge request is unavailable")
var errHostMergeNoView = errors.New("Host merge request has no visible view")
var errHostRevisionMoved = errors.New("Host Projection Revision moved")

type projectionPullRequest struct {
	Number            int    `json:"number"`
	URL               string `json:"url"`
	HeadRefOID        string `json:"headRefOid"`
	HeadRefName       string `json:"headRefName"`
	BaseRefName       string `json:"baseRefName"`
	IsCrossRepository *bool  `json:"isCrossRepository"`
}

func validateProjectionPR(repo, branch string, pr projectionPullRequest) error {
	if pr.Number <= 0 || strings.TrimSpace(pr.URL) == "" {
		return fmt.Errorf("%w: pull request for branch %q has an incomplete number or URL",
			errHostMergeUnavailable, branch)
	}
	if !validHex(pr.HeadRefOID, 20) {
		return fmt.Errorf("%w: pull request for branch %q has an invalid head Revision",
			errHostMergeUnavailable, branch)
	}
	if pr.IsCrossRepository == nil {
		return fmt.Errorf("%w: pull request for branch %q has no cross-repository identity",
			errHostMergeUnavailable, branch)
	}
	if *pr.IsCrossRepository || pr.HeadRefName != branch || pr.BaseRefName != "master" {
		return fmt.Errorf("%w: pull request %d does not originate from %s branch %q and target master",
			errHostMergeUnavailable, pr.Number, repo, branch)
	}
	return nil
}

func projectionPRs(cfg Config, branch string) ([]projectionPullRequest, error) {
	out, err := projectionCommand("pr", "list", "-R", cfg.Repo, "--state", "open",
		"--head", branch, "--json", "number,url,headRefOid,headRefName,baseRefName,isCrossRepository")
	if err != nil {
		return nil, err
	}
	var prs []projectionPullRequest
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("%w: decode open Projection response: %v", errHostMergeUnavailable, err)
	}
	if prs == nil {
		return nil, fmt.Errorf("%w: decode open Projection response: expected a JSON array",
			errHostMergeUnavailable)
	}
	for _, pr := range prs {
		if err := validateProjectionPR(cfg.Repo, branch, pr); err != nil {
			return nil, err
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

type projectionReview struct {
	Body     string `json:"body"`
	CommitID string `json:"commit_id"`
}

func projectionCommentExists(cfg Config, number int, revision, body string) (bool, error) {
	out, err := projectionCommand("api", "--method", "GET", "--paginate", "--slurp",
		fmt.Sprintf("repos/%s/pulls/%d/reviews", cfg.Repo, number),
		"--field", "per_page=100")
	if err != nil {
		return false, err
	}
	var pages [][]*projectionReview
	if err := json.Unmarshal(out, &pages); err != nil {
		return false, fmt.Errorf("%w: decode Projection review response: %v", errHostMergeUnavailable, err)
	}
	if pages == nil {
		return false, fmt.Errorf("%w: decode Projection review response: expected a JSON array",
			errHostMergeUnavailable)
	}
	for _, page := range pages {
		if page == nil {
			return false, fmt.Errorf("%w: decode Projection review response: expected JSON array pages",
				errHostMergeUnavailable)
		}
		for _, review := range page {
			if review == nil || !validHex(review.CommitID, 20) {
				return false, fmt.Errorf("%w: decode Projection review response: invalid review identity",
					errHostMergeUnavailable)
			}
			if review.CommitID == revision && review.Body == body {
				return true, nil
			}
		}
	}
	return false, nil
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
		return nil, fmt.Errorf("%w: decode merged Projection response: %v", errHostMergeUnavailable, err)
	}
	if pages == nil {
		return nil, fmt.Errorf("%w: decode merged Projection response: expected a JSON array",
			errHostMergeUnavailable)
	}
	var prs []projectionPullRequest
	for _, page := range pages {
		if page == nil {
			return nil, fmt.Errorf("%w: decode merged Projection response: expected JSON array pages",
				errHostMergeUnavailable)
		}
		for _, raw := range page {
			crossRepository := raw.Head.Repo == nil || raw.Head.Repo.FullName != cfg.Repo
			pr := projectionPullRequest{
				Number:            raw.Number,
				URL:               raw.URL,
				HeadRefOID:        raw.Head.SHA,
				HeadRefName:       raw.Head.Ref,
				BaseRefName:       raw.Base.Ref,
				IsCrossRepository: &crossRepository,
			}
			if err := validateProjectionPR(cfg.Repo, branch, pr); err != nil {
				return nil, err
			}
			if raw.MergedAt == nil || *raw.MergedAt == "" {
				continue
			}
			if _, err := time.Parse(time.RFC3339, *raw.MergedAt); err != nil {
				return nil, fmt.Errorf("%w: decode merged Projection response: invalid merged timestamp",
					errHostMergeUnavailable)
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
		return nil, fmt.Errorf("%w: multiple open pull requests for branch %q",
			errHostMergeUnavailable, branch)
	}
	return prs, nil
}

// projectBranch publishes a built branch as one idempotent pull request.
// expectedHead fences every Host view and the remote source branch to one Revision.
func projectBranch(cfg Config, repoDir string, it Item, branch, body, expectedHead string) (string, bool, error) {
	if !cfg.Projection.Enabled {
		return "", false, nil
	}
	if expectedHead == "" {
		return "", false, fmt.Errorf("%w: Projection for branch %q requires a Revision", errHostMergeUnavailable, branch)
	}
	prs, err := openProjectionPR(cfg, branch)
	if err != nil {
		return "", false, err
	}
	if len(prs) > 0 {
		if prs[0].HeadRefOID != expectedHead {
			return "", false, fmt.Errorf("%w: %w: open pull request for branch %q moved to %s after reviewed Revision %s",
				errHostMergeUnavailable, errHostRevisionMoved, branch, prs[0].HeadRefOID, expectedHead)
		}
		return prs[0].URL, false, nil
	}
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
		return "", false, fmt.Errorf("%w: %w: merged pull request for branch %q does not match reviewed Revision %s",
			errHostMergeUnavailable, errHostRevisionMoved, branch, expectedHead)
	}
	head, err := branchHead(repoDir, branch)
	if err != nil {
		return "", false, fmt.Errorf("inspect Projection branch %q: %w", branch, err)
	}
	if head != expectedHead {
		return "", false, fmt.Errorf("%w: %w: branch %q moved to %s before Projection creation for Revision %s",
			errHostMergeUnavailable, errHostRevisionMoved, branch, head, expectedHead)
	}
	if err := claimEffect(repoDir, "Projection-create", branch, expectedHead); err != nil {
		return "", false, err
	}
	created, err := projectionCommand("pr", "create", "-R", cfg.Repo,
		"--base", "master", "--head", branch,
		"--title", redactSecretShaped("forest: "+it.Title),
		"--body", redactSecretShaped(body))
	if err != nil {
		return "", false, fmt.Errorf("%w: create Projection for branch %q: %v",
			errHostMergePending, branch, err)
	}
	createdURL := strings.TrimSpace(string(created))
	if createdURL == "" {
		return "", false, fmt.Errorf("%w: Host created a Projection without reporting its URL", errHostMergeUnavailable)
	}
	prs, err = openProjectionPR(cfg, branch)
	if err != nil {
		if errors.Is(err, errHostMergeUnavailable) {
			return "", false, err
		}
		return "", false, fmt.Errorf("%w: inspect created Projection: %v", errHostMergePending, err)
	}
	if len(prs) != 1 {
		return "", false, fmt.Errorf("%w: created Projection for branch %q is not visible", errHostMergePending, branch)
	}
	if prs[0].HeadRefOID != expectedHead {
		return "", false, fmt.Errorf("%w: %w: created Projection for branch %q reports Revision %s, want %s",
			errHostMergeUnavailable, errHostRevisionMoved, branch, prs[0].HeadRefOID, expectedHead)
	}
	if prs[0].URL != createdURL {
		return "", false, fmt.Errorf("%w: created Projection for branch %q reports URL %q, want %q",
			errHostMergeUnavailable, branch, prs[0].URL, createdURL)
	}
	return createdURL, false, nil
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
// expectedHead fences the comment to the exact Revision its evidence describes.
func projectComment(cfg Config, repoDir, branch, expectedHead, body string) error {
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
	if expectedHead == "" || prs[0].HeadRefOID == "" {
		return fmt.Errorf("%w: pull request for branch %q lacks the evidence Revision %s",
			errHostMergeUnavailable, branch, expectedHead)
	}
	if prs[0].HeadRefOID != expectedHead {
		return fmt.Errorf("%w: %w: pull request for branch %q moved to %s after evidence for Revision %s",
			errHostMergeUnavailable, errHostRevisionMoved, branch, prs[0].HeadRefOID, expectedHead)
	}
	body = redactSecretShaped(fmt.Sprintf("Revision: `%s`\n\n%s", expectedHead, body))
	exists, err := projectionCommentExists(cfg, prs[0].Number, expectedHead, body)
	if err != nil {
		if errors.Is(err, errHostMergeUnavailable) {
			return err
		}
		return fmt.Errorf("%w: inspect Projection comments: %v", errHostMergePending, err)
	}
	if exists {
		return nil
	}
	if err := claimEffect(repoDir, "Projection-comment", branch, expectedHead); err != nil {
		return err
	}
	_, publishErr := projectionCommand("api", "--method", "POST",
		fmt.Sprintf("repos/%s/pulls/%d/reviews", cfg.Repo, prs[0].Number),
		"--field", "event=COMMENT", "--field", "commit_id="+expectedHead,
		"--field", "body="+body)
	if publishErr == nil {
		return nil
	}
	exists, reconcileErr := projectionCommentExists(cfg, prs[0].Number, expectedHead, body)
	if reconcileErr == nil && exists {
		return nil
	}
	if reconcileErr != nil {
		if errors.Is(reconcileErr, errHostMergeUnavailable) {
			return reconcileErr
		}
		return fmt.Errorf("%w: publish Projection comment: %v; reconcile: %v",
			errHostMergeUnavailable, publishErr, reconcileErr)
	}
	return fmt.Errorf("%w: publish Projection comment: %v", errHostMergeUnavailable, publishErr)
}

// projectVerdict mirrors a git Verdict and its Checks as one pull-request comment.
func projectVerdict(cfg Config, repoDir, branch, expectedHead string, v verdictNote, c checksNote) error {
	return projectComment(cfg, repoDir, branch, expectedHead, verdictBody(v, c))
}

// projectChecks mirrors a failing Checks result on the human surface. The
// Verifier stops before review when a check fails, so this is the only signal
// an operator would otherwise get for that Revision.
func projectChecks(cfg Config, repoDir, branch, expectedHead string, c checksNote) error {
	return projectComment(cfg, repoDir, branch, expectedHead,
		checksSummary(c)+"\n\n"+verdictBody(verdictNote{Verdict: "pending"}, c))
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
		"-R", cfg.Repo, "--squash", "--match-head-commit", expectedHead}
	if _, err := projectionCommand(args...); err != nil {
		return fmt.Errorf("%w: %w: %v", errHostMergePending, errHostMergeRequestFailed, err)
	}
	merged, _, err = inspectProjectMerge(cfg, branch, strategy, expectedHead)
	if err != nil {
		// The command completed successfully. Persist that accepted Effect even
		// when the immediate Host view is unavailable.
		return fmt.Errorf("%w: %w: post-request state: %v",
			errHostMergePending, errHostMergeAccepted, err)
	}
	if !merged {
		return fmt.Errorf("%w: %w", errHostMergePending, errHostMergeAccepted)
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
	if expectedHead == "" {
		return false, projectionPullRequest{}, fmt.Errorf("%w: Host merge for branch %q requires a Revision",
			errHostMergeUnavailable, branch)
	}
	merged, err := mergedProjectionPRs(cfg, branch)
	if err != nil {
		return false, projectionPullRequest{}, err
	}
	for _, pr := range merged {
		if pr.HeadRefOID == expectedHead {
			return true, pr, nil
		}
	}
	prs, err := openProjectionPR(cfg, branch)
	if err != nil {
		return false, projectionPullRequest{}, err
	}
	if len(prs) == 0 {
		if len(merged) > 0 {
			return false, projectionPullRequest{}, fmt.Errorf("%w: %w: merged pull request for branch %q does not match reviewed Revision %s",
				errHostMergeUnavailable, errHostRevisionMoved, branch, expectedHead)
		}
		return false, projectionPullRequest{}, fmt.Errorf("%w: no open or merged pull request for branch %q",
			errHostMergeNoView, branch)
	}
	pr := prs[0]
	if pr.HeadRefOID == "" {
		return false, projectionPullRequest{}, fmt.Errorf("%w: open pull request for branch %q has no reported head Revision",
			errHostMergeUnavailable, branch)
	}
	if pr.HeadRefOID != expectedHead {
		return false, projectionPullRequest{}, fmt.Errorf("%w: %w: open pull request for branch %q moved to %s after reviewed Revision %s",
			errHostMergeUnavailable, errHostRevisionMoved, branch, pr.HeadRefOID, expectedHead)
	}
	return false, pr, nil
}
