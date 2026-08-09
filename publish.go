package main

import "fmt"

// commitAndPush makes the deterministic commit for one run and pushes the
// branch. The agent never touches GitHub; this is the only writer.
//
// expectedSHA is the commit the caller observed at the remote branch tip. When
// it is set, the push uses Git's compare-and-swap flag, so a rewritten branch
// can land while still losing to any concurrent writer. An empty value keeps
// the plain push a new branch needs.
func commitAndPush(repo, wtDir, branch, expectedSHA string, id CommitIdentity, it Item) (string, error) {
	if err := git(wtDir, "add", "-A"); err != nil {
		return "", err
	}
	// keep report.json and review.json out of the tree: they are the run's
	// records, not the repo's change
	_ = git(wtDir, "reset", "-q", "--", "report.json", "review.json")
	if err := gitCommit(wtDir, id, fmt.Sprintf("forest: %s (#%s)", it.Title, it.ID)); err != nil {
		return "", err
	}
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	args := []string{"push", "-u", "origin", branch}
	if expectedSHA != "" {
		args = []string{"push", "--force-with-lease=refs/heads/" + branch + ":" + expectedSHA, "origin", branch}
	}
	if err := git(repo, args...); err != nil {
		return "", err
	}
	return head, nil
}
