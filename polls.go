package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

type toolFunc func(context.Context, string, ...string) ([]byte, error)

type Poller struct {
	Root         string
	Repo         string
	Scope        Scope
	Run          toolFunc
	ResolveTools bool
}

func NewPoller(root, repo string, scope Scope) *Poller {
	return &Poller{Root: root, Repo: repo, Scope: scope, Run: realTool, ResolveTools: true}
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

func (p *Poller) powder(ctx context.Context, args ...string) ([]byte, error) {
	return p.run(ctx, "powder", args...)
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

const defaultReadyLabel = "forest:ready"

func (p *Poller) readyIssues(ctx context.Context, label string) ([]string, error) {
	if strings.TrimSpace(p.Repo) == "" {
		return nil, fmt.Errorf("repository is required")
	}
	endpoint := fmt.Sprintf("repos/%s/issues?state=open&labels=%s&per_page=100", p.Repo, url.QueryEscape(label))
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
	issues := make([]string, 0)
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
			issues = append(issues, strconv.Itoa(*issue.Number))
		}
	}
	sort.Strings(issues)
	return issues, nil
}

// builder reports whether a Subject is ready for a Builder Run. A non-nil error
// accompanies exitError so the caller can say why the Poll failed instead of
// exiting silently.
func (p *Poller) builder(ctx context.Context) (int, error) {
	issues, err := p.issueSubjects(ctx)
	if err != nil {
		return exitError, err
	}
	for _, subject := range issues {
		claimed, err := p.subjectHasBranch(ctx, subject)
		if err != nil {
			return exitError, err
		}
		if !claimed {
			return exitOK, nil
		}
	}
	powder, err := p.powderSubjects(ctx)
	if err != nil {
		return exitError, err
	}
	for _, subject := range powder {
		claimed, err := p.subjectHasBranch(ctx, subject)
		if err != nil {
			return exitError, err
		}
		if !claimed {
			return exitOK, nil
		}
	}
	return exitNoWork, nil
}

func (p *Poller) issueSubjects(ctx context.Context) ([]string, error) {
	label := defaultReadyLabel
	if p.Scope.Label != "" {
		label = p.Scope.Label
	}
	issues, err := p.readyIssues(ctx, label)
	if err != nil {
		return nil, err
	}
	return p.filterScopeSubjects(issues), nil
}

func (p *Poller) powderSubjects(ctx context.Context) ([]string, error) {
	// A label scope is a GitHub-only selection rule.
	if p.Scope.Label != "" {
		return nil, nil
	}
	powder, err := p.listPowderSubjects(ctx)
	if err != nil {
		return nil, err
	}
	return p.filterScopeSubjects(powder), nil
}

func (p *Poller) filterScopeSubjects(subjects []string) []string {
	switch {
	case len(p.Scope.Subjects) > 0:
		allowed := make(map[string]struct{}, len(p.Scope.Subjects))
		for _, subject := range p.Scope.Subjects {
			allowed[subject] = struct{}{}
		}
		return filterSubjects(subjects, func(subject string) bool {
			_, ok := allowed[subject]
			return ok
		})
	case p.Scope.BranchPrefix != "":
		prefix := p.Scope.BranchPrefix
		return filterSubjects(subjects, func(subject string) bool {
			return strings.HasPrefix("forest/"+subject, prefix)
		})
	default:
		return subjects
	}
}

func filterSubjects(subjects []string, keep func(string) bool) []string {
	result := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		if keep(subject) {
			result = append(result, subject)
		}
	}
	return result
}

func powderAgent() string {
	return strings.TrimSpace(os.Getenv("POWDER_AGENT"))
}

func powderOriginSet() bool {
	return strings.TrimSpace(os.Getenv("POWDER_URL")) != "" || strings.TrimSpace(os.Getenv("POWDER_API_BASE_URL")) != ""
}

type powderListJob struct {
	ID string `json:"id"`
}

func (p *Poller) listPowderSubjects(ctx context.Context) ([]string, error) {
	agent := powderAgent()
	if agent == "" {
		return nil, nil
	}
	if !powderOriginSet() {
		return nil, fmt.Errorf("POWDER_AGENT is set but POWDER_URL and POWDER_API_BASE_URL are empty")
	}
	if strings.TrimSpace(p.Repo) == "" {
		return nil, fmt.Errorf("repository is required")
	}
	takeable, err := p.listPowder(ctx, "--takeable", "--repo", p.Repo)
	if err != nil {
		return nil, err
	}
	mine, err := p.listPowder(ctx, "--mine", agent, "--repo", p.Repo)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(takeable)+len(mine))
	ids := make([]string, 0, len(takeable)+len(mine))
	for _, id := range append(takeable, mine...) {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !validSubject(id) {
			return nil, fmt.Errorf("malformed powder job id %q", id)
		}
	}
	return ids, nil
}

func (p *Poller) listPowder(ctx context.Context, args ...string) ([]string, error) {
	output, err := p.powder(ctx, append([]string{"list"}, args...)...)
	if err != nil {
		return nil, err
	}
	var jobs []powderListJob
	if err := json.Unmarshal(output, &jobs); err != nil {
		return nil, fmt.Errorf("malformed powder list: %w", err)
	}
	ids := make([]string, 0, len(jobs))
	for _, job := range jobs {
		id := strings.TrimSpace(job.ID)
		if id == "" {
			return nil, fmt.Errorf("malformed powder job id")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (p *Poller) subjectHasBranch(ctx context.Context, subject string) (bool, error) {
	output, err := p.git(ctx, "ls-remote", "--heads", "origin", "refs/heads/forest/"+subject+"/*")
	if err != nil {
		return false, err
	}
	matches, err := parseBranchOutput(output, subject)
	if err != nil {
		return false, err
	}
	return len(matches) > 0, nil
}

func parseBranchOutput(output []byte, subject string) ([]branchTip, error) {
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
		if subject != "" {
			if branchBelongsToSubject(branch, subject) {
				branches = append(branches, branchTip{Name: branch, SHA: fields[0]})
				continue
			}
			if validForestBranch(branch) || !strings.HasPrefix(branch, "forest/"+subject+"/") {
				continue
			}
			return nil, fmt.Errorf("branch does not match subject %s", subject)
		}
		if validForestBranch(branch) {
			branches = append(branches, branchTip{Name: branch, SHA: fields[0]})
		}
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
	return parseBranchOutput(output, "")
}

const pollNotesNamespace = "refs/notes/forest-poll"

var pollMissingNote = errors.New("missing coordination note")

func isMissingNote(err error) bool {
	return errors.Is(err, pollMissingNote)
}
