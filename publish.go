package main

import (
	"fmt"
	"strings"
)

// commitAndPush makes the deterministic commit for one run and pushes the
// branch. The agent never touches GitHub; this is the only writer.
//
// expectedSHA is the commit the caller observed at the remote branch tip. When
// it is set, the push uses Git's compare-and-swap flag, so a rewritten branch
// can land while still losing to any concurrent writer. An empty value keeps
// the plain push a new branch needs.
//
// Before it writes anything, checkPublishOrigin confirms the push target is the
// configured repository, so a run never publishes a branch to an origin it does
// not own. After the push, readBackPushedHead confirms the remote branch now
// points at the exact commit this run made; a publish that lands anywhere it was
// not meant to is an error, never a recorded success.
func commitAndPush(cfg Config, repo, wtDir, branch, expectedSHA string, id CommitIdentity, it Item) error {
	if err := checkPublishOrigin(repo, cfg.Repo); err != nil {
		return err
	}
	if err := git(wtDir, "add", "-A"); err != nil {
		return err
	}
	// keep report.json and review.json out of the tree: they are the run's
	// records, not the repo's change
	_ = git(wtDir, "reset", "-q", "--", "report.json", "review.json")
	if err := gitCommit(wtDir, id, fmt.Sprintf("forest: %s (#%s)", it.Title, it.ID)); err != nil {
		return err
	}
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("publish: read committed head: %w", err)
	}
	if err := assertPublishableTree(wtDir); err != nil {
		return err
	}
	args := []string{"push", "-u", "origin", branch}
	if expectedSHA != "" {
		args = []string{"push", "--force-with-lease=refs/heads/" + branch + ":" + expectedSHA, "origin", branch}
	}
	if err := git(repo, args...); err != nil {
		return err
	}
	return readBackPushedHead(repo, branch, head)
}

// checkPublishOrigin refuses a publish the moment the configured origin does not
// identify the repository the factory is declared to work on. A force-with-lease
// push only protects against a lost race; it does nothing against publishing the
// branch to a different remote, so the origin is verified before anything is
// staged or uploaded.
func checkPublishOrigin(repoDir, want string) error {
	url, err := gitOut(repoDir, "remote", "get-url", "origin")
	if err != nil {
		return fmt.Errorf("publish: read origin: %w", err)
	}
	if !sameRepoURL(url, want) {
		return fmt.Errorf("publish refused: origin %q is not the configured repo %q", url, want)
	}
	return nil
}

// sameRepoURL reports whether a git remote URL identifies the same repository as
// the configured owner/name string, across the transport forms git and the host
// accept: SSH "git@host:owner/repo", HTTPS "https://host/owner/repo", scp-style,
// and the bare "owner/repo". Each is reduced to its trailing "owner/name" path,
// discarding any scheme, scp host, leading host segment, and ".git" suffix. An
// empty want matches no URL, so a missing configured repo can never be reached.
func sameRepoURL(remoteURL, want string) bool {
	if want == "" {
		return false
	}
	return repoPathOfURL(remoteURL) == repoPathOfURL(want)
}

// repoPathOfURL reduces a git remote URL to the trailing repository path it
// names, discarding any transport scheme, scp host, and ".git" suffix, so two
// URLs that reach the same repository compare equal.
func repoPathOfURL(raw string) string {
	u := strings.TrimSpace(raw)
	u = strings.TrimSuffix(u, ".git")
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
	}
	if at := strings.IndexByte(u, '@'); at >= 0 {
		u = u[at+1:]
	}
	if i := strings.IndexByte(u, ':'); i >= 0 {
		u = u[i+1:]
	}
	u = strings.TrimPrefix(u, "/")
	parts := strings.Split(u, "/")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return strings.Join(parts, "/")
}

// assertPublishableTree refuses a run that left anything but its own records in
// the tree after the commit. report.json and review.json are the only untracked
// residue a publish may leave behind, so any other dirty path means the commit
// was built from a tree that is not the agent's real change and the publish is
// not recorded as success.
func assertPublishableTree(wtDir string) error {
	out, err := gitOutRaw(wtDir, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("publish: status: %w", err)
	}
	for _, path := range parseChanged(out) {
		if isRunArtifact(path) {
			continue
		}
		return fmt.Errorf("publish refused: worktree is dirty behind the change: %q", path)
	}
	return nil
}

// readBackPushedHead confirms the remote branch now points at the exact commit
// this run pushed. The lease makes the push atomic, but it never proves where
// the branch landed; reading the tip back and comparing it to the pushed SHA is
// what lets an unattended run trust that local and remote agree on one commit.
// A mismatch is an error, never a recorded success.
func readBackPushedHead(repo, branch, pushedSHA string) error {
	if err := git(repo, "fetch", "origin", branch); err != nil {
		return fmt.Errorf("publish: read back %s: %w", branch, err)
	}
	remote, err := gitOut(repo, "rev-parse", "origin/"+branch)
	if err != nil {
		return fmt.Errorf("publish: read back %s: %w", branch, err)
	}
	if remote != pushedSHA {
		return fmt.Errorf("publish refused: after pushing, remote %s is %s, not the pushed %s", branch, short(remote), short(pushedSHA))
	}
	return nil
}
