package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoPathOfURL pins how sameRepoURL compares remote URLs across the
// transport forms git and the host accept, by checking the trailing repository
// path each form reduces to.
func TestRepoPathOfURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"git@github.com:owner/repo.git", "owner/repo"},
		{"https://github.com/owner/repo.git", "owner/repo"},
		{"https://github.com/owner/repo", "owner/repo"},
		{"ssh://git@github.com/owner/repo.git", "owner/repo"},
		{"owner/repo", "owner/repo"},
		{"owner/repo.git", "owner/repo"},
		{"/tmp/cache/owner/repo.git", "owner/repo"},
	}
	for _, c := range cases {
		if got := repoPathOfURL(c.url); got != c.want {
			t.Fatalf("repoPathOfURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}

// publishTestOrigin sets up a bare origin at a path that names owner/repo and a
// work clone of it, so the configured repo matches the origin and pushes reach a
// real remote. It returns the work directory.
func publishTestOrigin(t *testing.T, owner, repo string) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, owner, repo+".git")
	work := filepath.Join(root, "work")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
		}
	}
	if err := os.MkdirAll(filepath.Dir(origin), 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "--bare", "--initial-branch=master", origin)
	run("init", "--initial-branch=master", work)
	run("-C", work, "config", "user.email", "publish-test@example.com")
	run("-C", work, "config", "user.name", "publish-test")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("-C", work, "add", "file.txt")
	run("-C", work, "commit", "-m", "first")
	run("-C", work, "remote", "add", "origin", origin)
	run("-C", work, "push", "-q", "-u", "origin", "master")
	return work
}

// TestCheckPublishOriginRefusesWrongRemote pins the origin precondition: a
// publish against a remote that is not the configured repository is refused, and
// a matching remote is accepted.
func TestCheckPublishOriginRefusesWrongRemote(t *testing.T) {
	work := publishTestOrigin(t, "owner", "repo")
	if err := checkPublishOrigin(work, "owner/repo"); err != nil {
		t.Fatalf("matching origin refused: %v", err)
	}
	if err := checkPublishOrigin(work, "someone/else"); err == nil {
		t.Fatal("a wrong origin was accepted")
	}
}

// TestPublishDirtyTreeRefuses pins the dirty-tree precondition: a run that leaves
// anything but its own records behind the change is refused, while the run's
// records (report.json, review.json) are allowed residue.
func TestPublishDirtyTreeRefuses(t *testing.T) {
	work := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", work}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
		}
	}
	run("init", "-q")
	run("config", "user.email", "publish-test@example.com")
	run("config", "user.name", "publish-test")
	if err := os.WriteFile(filepath.Join(work, "report.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assertPublishableTree(work); err != nil {
		t.Fatalf("run records made the tree unpublishable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(work, "stray.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := assertPublishableTree(work); err == nil {
		t.Fatal("a stray dirty path was not refused")
	}
}

// TestReadBackPushedHead pins the read-back precondition: after a push the remote
// head is compared to the pushed SHA, and a mismatch is refused.
func TestReadBackPushedHead(t *testing.T) {
	work := publishTestOrigin(t, "owner", "repo")
	head, err := gitOut(work, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := readBackPushedHead(work, "master", head); err != nil {
		t.Fatalf("reading back the pushed head = %v, want nil", err)
	}
	other := strings.Repeat("0", 40)
	if err := readBackPushedHead(work, "master", other); err == nil {
		t.Fatal("a read-back that differs from the pushed SHA was accepted")
	}
}

// TestCommitAndPushRefusesWrongOrigin runs the public publish path end to end and
// pins that its first refusal is the origin: nothing is staged or uploaded when
// the remote is not the configured repository.
func TestCommitAndPushRefusesWrongOrigin(t *testing.T) {
	work := publishTestOrigin(t, "owner", "repo")
	branch := "forest/9-x"
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", work}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
		}
	}
	run("checkout", "-q", "-b", branch)
	it := Item{ID: "9", Title: "x"}
	id := CommitIdentity{Name: "publish-test", Email: "publish-test@example.com"}
	cfg := defaultConfig()
	cfg.Repo = "someone/else"
	if err := commitAndPush(cfg, work, work, branch, "", id, it); err == nil {
		t.Fatal("commitAndPush published to a wrong origin")
	}
	// Nothing was pushed: the branch must not exist on the remote.
	out, err := gitOut(work, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "" {
		t.Fatalf("wrong-origin publish left a branch on the remote: %s", out)
	}
}
