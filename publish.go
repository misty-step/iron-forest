package main

import "fmt"

// commitAndPush makes the deterministic commit for one run and pushes the
// branch. The agent never touches GitHub; this is the only writer.
func commitAndPush(repo, wtDir, branch string, it issue) error {
	if err := git(wtDir, "add", "-A"); err != nil {
		return err
	}
	// keep report.json and review.json out of the tree: they are the run's
	// records, not the repo's change
	_ = git(wtDir, "reset", "-q", "--", "report.json", "review.json")
	if err := gitCommit(wtDir, fmt.Sprintf("forest: %s (#%d)", it.Title, it.Number)); err != nil {
		return err
	}
	if err := git(repo, "push", "-u", "origin", branch); err != nil {
		return err
	}
	return nil
}
