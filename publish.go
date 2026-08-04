package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// commitAndPush makes the deterministic commit for one run and pushes the
// branch. The agent never touches GitHub; this is the only writer.
func commitAndPush(repo, wtDir, branch string, it issue) error {
	if err := git(wtDir, "add", "-A"); err != nil {
		return err
	}
	// keep report.json out of the tree: it is the run's record, not the repo's
	_ = git(wtDir, "reset", "-q", "--", "report.json")
	if err := gitCommit(wtDir, fmt.Sprintf("forest: %s (#%d)", it.Title, it.Number)); err != nil {
		return err
	}
	if err := git(repo, "push", "-u", "origin", branch); err != nil {
		return err
	}
	return nil
}

// openPR creates the pull request deterministically and idempotently: if a PR
// for the head branch already exists it returns it instead of a second one.
func openPR(repo, head, title, body string) (string, error) {
	out, err := ghJSON("pr", "list", "-R", repo, "--state", "open",
		"--head", head, "--json", "url")
	if err != nil {
		return "", err
	}
	var existing []struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(out, &existing); err != nil {
		return "", err
	}
	if len(existing) > 0 {
		return existing[0].URL, nil
	}
	created, err := ghJSON("pr", "create", "-R", repo,
		"--base", "master", "--head", head, "--title", title, "--body", body)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(created)), nil
}
