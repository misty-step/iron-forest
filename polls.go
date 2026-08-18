package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

type toolFunc func(context.Context, string, ...string) ([]byte, error)

type Poller struct {
	Root         string
	Repo         string
	Run          toolFunc
	ResolveTools bool
}

func NewPoller(root, repo string) *Poller {
	return &Poller{Root: root, Repo: repo, Run: realTool, ResolveTools: true}
}

func realTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	return processGroupOutput(ctx, exec.Command(name, args...))
}

func (p *Poller) git(ctx context.Context, args ...string) ([]byte, error) {
	return p.run(ctx, "git", append([]string{"-C", p.Root}, args...)...)
}

func (p *Poller) gh(ctx context.Context, args ...string) ([]byte, error) {
	return p.run(ctx, "gh", args...)
}

func (p *Poller) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if p.ResolveTools {
		path, err := trustedExecutable(p.Root, name)
		if err != nil {
			return nil, err
		}
		name = path
	}
	return p.Run(ctx, name, args...)
}

type trackerIssue struct {
	Number      *int            `json:"number"`
	PullRequest json.RawMessage `json:"pull_request"`
}

func (p *Poller) readyIssues(ctx context.Context) ([]int, error) {
	if strings.TrimSpace(p.Repo) == "" {
		return nil, fmt.Errorf("repository is required")
	}
	endpoint := fmt.Sprintf("repos/%s/issues?state=open&labels=forest%%3Aready&per_page=100", p.Repo)
	output, err := p.gh(ctx, "api", "--paginate", "--slurp", endpoint)
	if err != nil {
		return nil, err
	}
	var pages [][]trackerIssue
	if err := json.Unmarshal(output, &pages); err != nil {
		return nil, fmt.Errorf("malformed issue output: %w", err)
	}
	if pages == nil {
		return nil, fmt.Errorf("malformed issue output")
	}
	issues := make([]int, 0)
	for _, pageIssues := range pages {
		for _, issue := range pageIssues {
			if issue.Number == nil || *issue.Number <= 0 {
				return nil, fmt.Errorf("malformed issue number")
			}
			pullRequest := strings.TrimSpace(string(issue.PullRequest))
			if pullRequest != "" && pullRequest != "null" {
				var object map[string]json.RawMessage
				if !strings.HasPrefix(pullRequest, "{") || json.Unmarshal(issue.PullRequest, &object) != nil || object == nil {
					return nil, fmt.Errorf("malformed pull request field")
				}
				continue
			}
			issues = append(issues, *issue.Number)
		}
	}
	sort.Ints(issues)
	return issues, nil
}

// builder reports whether an Issue is ready for a Builder Run. A non-nil error
// accompanies exitError so the caller can say why the Poll failed instead of
// exiting silently.
func (p *Poller) builder(ctx context.Context) (int, error) {
	issues, err := p.readyIssues(ctx)
	if err != nil {
		return exitError, err
	}
	for _, issue := range issues {
		branches, err := p.git(ctx, "ls-remote", "--heads", "origin", fmt.Sprintf("refs/heads/forest/%d-*", issue))
		if err != nil {
			return exitError, err
		}
		matches, err := parseBranchOutput(branches, issue)
		if err != nil {
			return exitError, err
		}
		if len(matches) == 0 {
			return exitOK, nil
		}
	}
	return exitNoWork, nil
}

func parseBranchOutput(output []byte, issue int) ([]branchTip, error) {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return nil, nil
	}
	branches := make([]branchTip, 0)
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !isSHA(fields[0]) {
			return nil, fmt.Errorf("malformed branch ls-remote output")
		}
		ref := fields[1]
		if !strings.HasPrefix(ref, "refs/heads/") {
			return nil, fmt.Errorf("malformed branch ref")
		}
		branch := strings.TrimPrefix(ref, "refs/heads/")
		branchIssue := issue
		if branchIssue <= 0 {
			parts := strings.SplitN(strings.TrimPrefix(branch, "forest/"), "-", 2)
			parsedIssue, err := strconv.Atoi(parts[0])
			if err != nil || parsedIssue <= 0 {
				return nil, fmt.Errorf("malformed branch ref")
			}
			branchIssue = parsedIssue
		}
		if !validBranch(branch, branchIssue) {
			if issue > 0 {
				return nil, fmt.Errorf("branch does not match issue %d", issue)
			}
			return nil, fmt.Errorf("malformed branch ref")
		}
		branches = append(branches, branchTip{Name: branch, SHA: fields[0]})
	}
	return branches, nil
}

func (p *Poller) verifier(ctx context.Context) (code int, pollErr error) {
	branches, err := p.branchTips(ctx)
	if err != nil {
		return exitError, err
	}
	if len(branches) == 0 {
		return exitNoWork, nil
	}
	for _, tip := range branches {
		review, reviewErr := p.evidencePayload(ctx, "request", tip.SHA, "builder", "fixer")
		if reviewErr != nil {
			if !isMissingNote(reviewErr) {
				return exitError, reviewErr
			}
			continue
		}
		if err := validatePollReviewRequestBranch(review, tip.SHA, tip.Name); err != nil {
			return exitError, err
		}
		_, verdictErr := p.evidencePayload(ctx, "verdict", tip.SHA, "verifier")
		if verdictErr == nil {
			continue
		}
		if !isMissingNote(verdictErr) {
			return exitError, verdictErr
		}
		requestOID, err := p.remoteEvidenceOID(ctx, "request", tip.SHA)
		if err != nil {
			return exitError, err
		}
		if err := p.confirmEvidence(ctx, tip, requestOID, ""); err != nil {
			return exitError, err
		}
		return exitOK, nil
	}
	return exitNoWork, nil
}

func (p *Poller) fixer(ctx context.Context) (code int, pollErr error) {
	branches, err := p.branchTips(ctx)
	if err != nil {
		return exitError, err
	}
	if len(branches) == 0 {
		return exitNoWork, nil
	}
	for _, tip := range branches {
		verdict, verdictErr := p.evidencePayload(ctx, "verdict", tip.SHA, "verifier")
		if verdictErr != nil {
			if !isMissingNote(verdictErr) {
				return exitError, verdictErr
			}
			continue
		}
		parsed, err := decodeVerdict(verdict, tip.SHA)
		if err != nil {
			return exitError, err
		}
		if parsed.Verdict != "changes" {
			continue
		}
		review, reviewErr := p.evidencePayload(ctx, "request", tip.SHA, "builder", "fixer")
		if reviewErr != nil {
			return exitError, reviewErr
		}
		if err := validatePollReviewRequestBranch(review, tip.SHA, tip.Name); err != nil {
			return exitError, err
		}
		requestOID, err := p.remoteEvidenceOID(ctx, "request", tip.SHA)
		if err != nil {
			return exitError, err
		}
		verdictOID, err := p.remoteEvidenceOID(ctx, "verdict", tip.SHA)
		if err != nil {
			return exitError, err
		}
		if err := p.confirmEvidence(ctx, tip, requestOID, verdictOID); err != nil {
			return exitError, err
		}
		return exitOK, nil
	}
	return exitNoWork, nil
}

type branchTip struct {
	Name string
	SHA  string
}

func (p *Poller) branchTips(ctx context.Context) ([]branchTip, error) {
	output, err := p.git(ctx, "ls-remote", "--heads", "origin", "refs/heads/forest/*")
	if err != nil {
		return nil, err
	}
	return parseBranchOutput(output, 0)
}

const pollNotesNamespace = "refs/notes/forest-poll"

var pollMissingNote = errors.New("missing coordination note")

func isMissingNote(err error) bool {
	return errors.Is(err, pollMissingNote)
}
