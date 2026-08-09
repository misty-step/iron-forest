package main

import (
	"encoding/json"
	"fmt"
)

// commitAndPush makes the deterministic commit for one run and pushes the
// branch. The agent never touches GitHub; this is the only writer.
//
// expectedSHA is the commit the caller observed at the remote branch tip. When
// it is set, the push uses Git's compare-and-swap flag, so a rewritten branch
// can land while still losing to any concurrent writer. An empty value keeps
// the plain push a new branch needs.
func commitAndPush(repo, wtDir, branch, expectedSHA string, id CommitIdentity, it Item) (string, error) {
	head, err := commitWorktree(wtDir, id, it)
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

func commitFixAndPush(
	repo, wtDir, branch, expectedSHA, attemptKey string,
	expectedAttempts int,
	id CommitIdentity,
	it Item,
) (string, int, error) {
	head, err := commitWorktree(wtDir, id, it)
	if err != nil {
		return "", 0, err
	}
	attemptRef := "refs/forest/attempt/" + attemptKey
	attemptSHA, body, err := getBlobRef(repo, attemptRef)
	if err != nil {
		return "", 0, err
	}
	current := 0
	if attemptSHA != "" {
		current, err = decodeAttempts(body)
		if err != nil {
			return "", 0, err
		}
	}
	if current != expectedAttempts {
		return "", 0, fmt.Errorf("%w: Fixer attempts moved from %d to %d",
			errHostMergeUnavailable, expectedAttempts, current)
	}
	next := current + 1
	payload, err := json.Marshal(attemptsNote{Count: next})
	if err != nil {
		return "", 0, err
	}
	attemptBlob, err := writeBlob(repo, string(payload))
	if err != nil {
		return "", 0, err
	}
	args := []string{
		"push",
		"--atomic",
		"--force-with-lease=refs/heads/" + branch + ":" + expectedSHA,
		"--force-with-lease=" + attemptRef + ":" + attemptSHA,
		"origin",
		branch,
		attemptBlob + ":" + attemptRef,
	}
	if err := git(repo, args...); err != nil {
		remoteHead, present, headErr := lookupBranchHead(repo, branch)
		remoteAttempts, attemptsErr := readAttempts(repo, attemptKey)
		if headErr == nil && present && remoteHead == head &&
			attemptsErr == nil && remoteAttempts == next {
			return head, next, nil
		}
		return "", 0, err
	}
	return head, next, nil
}

func commitWorktree(wtDir string, id CommitIdentity, it Item) (string, error) {
	if err := git(wtDir, "add", "-A"); err != nil {
		return "", err
	}
	// Keep run reports outside the managed tree.
	_ = git(wtDir, "reset", "-q", "--", "report.json", "review.json")
	if err := gitCommit(wtDir, id, fmt.Sprintf("forest: %s (#%s)", it.Title, it.ID)); err != nil {
		return "", err
	}
	return gitOut(wtDir, "rev-parse", "HEAD")
}
