package main

import "fmt"

// commitAndPush makes the deterministic commit for one run and pushes the
// branch. The agent never touches GitHub; this is the only writer.
//
// lease is the commit the caller observed at the remote branch tip. When it is
// set, the push carries that lease, so a run that rewrote history — resolving a
// conflict by rebasing is the reason this exists — can land while still losing
// to any concurrent writer. An empty lease keeps the plain push a new branch
// needs.
func commitAndPush(repo, wtDir, branch, lease string, id CommitIdentity, it issue) error {
	if err := git(wtDir, "add", "-A"); err != nil {
		return err
	}
	// keep report.json and review.json out of the tree: they are the run's
	// records, not the repo's change
	_ = git(wtDir, "reset", "-q", "--", "report.json", "review.json")
	if err := gitCommit(wtDir, id, fmt.Sprintf("forest: %s (#%d)", it.Title, it.Number)); err != nil {
		return err
	}
	args := []string{"push", "-u", "origin", branch}
	if lease != "" {
		args = []string{"push", "--force-with-lease=refs/heads/" + branch + ":" + lease, "origin", branch}
	}
	if err := git(repo, args...); err != nil {
		return err
	}
	return nil
}
