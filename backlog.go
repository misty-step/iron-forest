package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// issue is one tracker item and its durable discussion.
type issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	UpdatedAt string    `json:"updatedAt"`
	Comments  []comment `json:"comments"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// comment is one tracker comment in source order.
type comment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

func (it issue) hasLabel(name string) bool {
	for _, label := range it.Labels {
		if label.Name == name {
			return true
		}
	}
	return false
}

// ghJSON runs the host CLI once. Tests replace it to stub the tracker without
// invoking the host binary.
var ghJSON = func(args ...string) ([]byte, error) {
	return exec.Command("gh", args...).Output()
}

func listOpenIssues(repo string) ([]issue, error) {
	out, err := ghJSON("issue", "list", "-R", repo, "--state", "open",
		"--json", "number,title,body,updatedAt,comments,labels", "--limit", "200")
	if err != nil {
		return nil, err
	}
	var items []issue
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func getItem(repo string, n int) (issue, error) {
	out, err := ghJSON("issue", "view", strconv.Itoa(n), "-R", repo,
		"--json", "number,title,body,updatedAt,comments,labels")
	if err != nil {
		return issue{}, err
	}
	var item issue
	if err := json.Unmarshal(out, &item); err != nil {
		return issue{}, err
	}
	return item, nil
}

func eligibleItems(cfg Config, repoDir string) ([]issue, error) {
	items, err := listOpenIssues(cfg.Repo)
	if err != nil {
		return nil, err
	}
	branches, err := forestBranches(repoDir)
	if err != nil {
		return nil, err
	}
	return eligibleFrom(items, branches, cfg.Flows.Builder.ExcludeLabels, cfg.Flows.Builder.RequireLabels), nil
}

func eligibleFrom(items []issue, branches, excluded, required []string) []issue {
	covered := make(map[int]bool)
	for _, branch := range branches {
		name := strings.TrimPrefix(strings.TrimPrefix(branch, "refs/heads/"), BranchPrefix)
		dash := strings.IndexByte(name, '-')
		if dash <= 0 {
			continue
		}
		n, err := strconv.Atoi(name[:dash])
		if err == nil {
			covered[n] = true
		}
	}
	var ready []issue
	for _, item := range items {
		if covered[item.Number] {
			continue
		}
		// Declared required labels turn selection into an opt-in: a promoter's
		// label earns an item its turn. Otherwise the opt-out contract applies,
		// so an item stays eligible unless an excluded label names it.
		if len(required) > 0 {
			if !hasLabels(item, required) {
				continue
			}
		} else if hasExcludedLabel(item, excluded) {
			continue
		}
		ready = append(ready, item)
	}
	return ready
}

func hasLabels(item issue, required []string) bool {
	for _, label := range required {
		if !item.hasLabel(label) {
			return false
		}
	}
	return true
}

func hasExcludedLabel(item issue, excluded []string) bool {
	for _, label := range excluded {
		if item.hasLabel(label) {
			return true
		}
	}
	return false
}

func forestBranches(repoDir string) ([]string, error) {
	out, err := gitCommand(repoDir, "ls-remote", "origin", "refs/heads/forest/*")
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "refs/heads/") {
			continue
		}
		branches = append(branches, strings.TrimPrefix(fields[1], "refs/heads/"))
	}
	return branches, nil
}

func branchHead(repoDir, branch string) (string, error) {
	ref := branch
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	out, err := gitCommand(repoDir, "ls-remote", "origin", ref)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("branch %s not found", branch)
	}
	return fields[0], nil
}

func commentItem(repo string, n int, body string) error {
	_, err := ghJSON("issue", "comment", "-R", repo, strconv.Itoa(n), "--body", body)
	return err
}

func closeItem(repo string, n int) error {
	_, err := ghJSON("issue", "close", "-R", repo, strconv.Itoa(n))
	return err
}

func labelItem(repo string, n int, add, remove []string) error {
	args := []string{"issue", "edit", "-R", repo, strconv.Itoa(n)}
	for _, label := range add {
		args = append(args, "--add-label", label)
	}
	for _, label := range remove {
		args = append(args, "--remove-label", label)
	}
	if len(args) == 5 {
		return nil
	}
	_, err := ghJSON(args...)
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
