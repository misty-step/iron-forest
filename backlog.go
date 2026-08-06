package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// issue is the unit of work. One open GitHub issue becomes one pull request.
type issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (it issue) hasLabel(name string) bool {
	for _, l := range it.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// ghJSON runs the gh CLI and returns its stdout. It is a package-level
// variable rather than a function so the offline tests can stub the CLI
// without a gh binary or a network.
var ghJSON = func(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	return cmd.Output()
}

func listOpenIssues(repo string) ([]issue, error) {
	out, err := ghJSON("issue", "list", "-R", repo, "--state", "open",
		"--json", "number,title,body,labels", "--limit", "200")
	if err != nil {
		return nil, err
	}
	var items []issue
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func getIssue(repo string, n int) (issue, error) {
	it, err := listOpenIssues(repo)
	if err != nil {
		return issue{}, err
	}
	for _, i := range it {
		if i.Number == n {
			return i, nil
		}
	}
	return issue{}, fmt.Errorf("issue %d is not open", n)
}

// getIssueAny fetches an issue in any state (the reaction loop re-enters
// issues that are already closed because their PR is open).
func getIssueAny(repo string, n int) (issue, error) {
	out, err := ghJSON("issue", "view", fmt.Sprintf("%d", n), "-R", repo,
		"--json", "number,title,body,labels")
	if err != nil {
		return issue{}, err
	}
	var it issue
	if err := json.Unmarshal(out, &it); err != nil {
		return issue{}, err
	}
	return it, nil
}

var prRefRe = regexp.MustCompile(`(?i)\b(?:fixes|closes|resolves)\s+#\d+`)

// openPRsReferencing finds whether an open PR mentions the issue.
func openPRsReferencing(repo string, n int) ([]string, error) {
	out, err := ghJSON("pr", "list", "-R", repo, "--state", "open",
		"--json", "number,title,body,url,headRefName", "--limit", "200")
	if err != nil {
		return nil, err
	}
	var prs []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		URL         string `json:"url"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("#%d", n)
	var hits []string
	for _, p := range prs {
		if strings.Contains(p.Body, ref) || strings.Contains(p.Title, ref) {
			hits = append(hits, fmt.Sprintf("#%d %s %s", p.Number, p.Title, p.URL))
		}
	}
	_ = prRefRe
	return hits, nil
}

// backlog is every open issue this loop may chew: open, unclaimed by forest,
// not failed by forest, and not already covered by an open pull request.
func backlog(cfg Config) ([]issue, error) {
	items, err := listOpenIssues(cfg.Repo)
	if err != nil {
		return nil, err
	}
	var ready []issue
	for _, it := range items {
		if it.hasLabel(claimLabel) || it.hasLabel(failedLabel) || it.hasLabel("parked") {
			continue
		}
		hits, err := openPRsReferencing(cfg.Repo, it.Number)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			continue
		}
		ready = append(ready, it)
	}
	return ready, nil
}

func ensureLabels(repo string) error {
	for _, label := range []string{claimLabel, failedLabel} {
		// --force keeps the call idempotent when the label already exists.
		_, _ = ghJSON("label", "create", "-R", repo, label, "--color", "a371f7", "--force")
	}
	return nil
}

// claimIssue parks the item so the loop never picks it a second time.
func claimIssue(repo string, n int) error {
	_, err := ghJSON("issue", "edit", "-R", repo, fmt.Sprintf("%d", n),
		"--add-label", claimLabel)
	return err
}

// failIssue records why a run failed, unparks the item, and marks it so the
// loop does not retry it forever.
func failIssue(repo string, n int, msg string) error {
	_, _ = ghJSON("issue", "comment", "-R", repo, fmt.Sprintf("%d", n),
		"--body", "forest run failed: "+msg)
	_, _ = ghJSON("issue", "edit", "-R", repo, fmt.Sprintf("%d", n),
		"--add-label", failedLabel, "--remove-label", claimLabel)
	return nil
}

// closeIssue completes the chewed item once its pull request is open. gh has
// no way to close through `issue edit`, so this uses the dedicated command.
func closeIssue(repo string, n int) error {
	_, _ = ghJSON("issue", "edit", "-R", repo, fmt.Sprintf("%d", n),
		"--remove-label", claimLabel, "--remove-label", failedLabel)
	_, err := ghJSON("issue", "close", "-R", repo, fmt.Sprintf("%d", n))
	return err
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	if out == "" {
		out = "item"
	}
	return strings.Trim(out, "-")
}
