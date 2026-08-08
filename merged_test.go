package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// closeTracker is a Tracker stub whose Close can be forced to fail (item #190):
// it records which items were closed so a test can prove a merged item was
// finalised and that a later reconciliation pass completed the job.
type closeTracker struct {
	items    []Item
	closed   []string
	closeErr error // when non-nil, every Close fails with this error
}

func (t *closeTracker) ListOpen() ([]Item, error)                     { return t.items, nil }
func (t *closeTracker) Get(id string) (Item, error)                   { return Item{ID: id}, nil }
func (t *closeTracker) Comment(id, body string) error                 { return nil }
func (t *closeTracker) Close(id string) error                         { t.closed = append(t.closed, id); return t.closeErr }
func (t *closeTracker) SetTags(id string, add, remove []string) error { return nil }

// TestMergedFactExcludesOpenItemFromBuilder proves the durable-fact guard of
// item #190: an open, unbranched item that already carries a merged fact is not
// eligible for the Builder, so the branch-deleted-but-item-open window never
// produces a duplicate build. Eligibility no longer depends on Tracker state
// alone.
func TestMergedFactExcludesOpenItemFromBuilder(t *testing.T) {
	work := setupTestRepo(t)

	old := trackerFor
	trackerFor = func(repo string) Tracker {
		return &closeTracker{items: []Item{{ID: "9", Title: "change"}}}
	}
	defer func() { trackerFor = old }()

	// Without the fact the open, unbranched item is eligible.
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	if got, err := eligibleItems(cfg, work); err != nil || len(got) != 1 {
		t.Fatalf("before fact: eligible = %+v, err=%v; want the one open item", got, err)
	}

	// Record the durable merged fact on the remote.
	if err := markMerged(work, "9", mergedNote{Branch: "forest/9-change", Revision: "deadbeef"}); err != nil {
		t.Fatalf("markMerged: %v", err)
	}

	items, err := eligibleItems(cfg, work)
	if err != nil {
		t.Fatalf("eligibleItems: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("merged item still eligible: %+v", items)
	}

	// The Builder selector, the real gate, must also refuse it.
	subjects, err := (builderFlow{}).Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range subjects {
		if s.ID == "9" {
			t.Fatalf("builder selected a merged item: %#v", s)
		}
	}
}

// TestMergeGitPathRecordsDurableMergedFact proves a successful git-path merge
// lands the merged fact atomically with the master update and branch deletion.
func TestMergeGitPathRecordsDurableMergedFact(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	oldGH := ghJSON
	ghJSON = func(args ...string) ([]byte, error) { return []byte(`{}`), nil }
	defer func() { ghJSON = oldGH }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}
	cfg.Flows.Verifier.Merge = "squash"
	cfg.Commit = CommitIdentity{Name: "forest-test", Email: "forest-test@example.com"}

	if err := mergeGitPath(cfg, repo, branch, reviewed, Item{ID: "9", Title: "change"}); err != nil {
		t.Fatalf("mergeGitPath: %v", err)
	}
	set, err := mergedIDs(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !set["9"] {
		t.Fatalf("merged fact for item 9 not found after the git-path merge: %v", set)
	}
	// The branch is gone and master advanced (the existing "lands" case already
	// pins those); the new invariant is the durable fact they are atomic with.
}

// TestFinishMergeSurvivesCloseFailureProves the order of item #190: with the
// Tracker Close forced to fail after the durable merged fact is recorded, the
// Builder no longer selects the item, and a later reconciliation pass completes
// the close (and removes the branch) without operator action.
func TestFinishMergeSurvivesCloseFailure(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	st := &closeTracker{items: []Item{{ID: "9", Title: "change"}}, closeErr: errors.New("host is down")}
	old := trackerFor
	trackerFor = func(string) Tracker { return st }
	defer func() { trackerFor = old }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}

	// First finalisation attempt: the merged fact lands, then Close fails.
	it := Item{ID: "9", Title: "change"}
	if err := finishMerge(cfg, repo, branch, reviewed, it); err == nil {
		t.Fatal("finishMerge with Close failing returned no error")
	}
	// The durable fact means the item is no longer eligible even though it is
	// still open and unbranched.
	if items, err := eligibleItems(cfg, repo); err != nil || len(items) != 0 {
		t.Fatalf("after failed close, eligible = %+v, err=%v; want none", items, err)
	}

	// The outage clears; the next pass reconciles and completes the finalisation.
	st.closeErr = nil
	if err := reconcileMerged(cfg, repo); err != nil {
		t.Fatalf("reconcileMerged: %v", err)
	}
	if len(st.closed) < 2 {
		t.Fatalf("item closed %v times, want the failed attempt plus the reconciliation close", st.closed)
	}
	if out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
		t.Fatalf("source branch %q still on origin after reconciliation: %s", branch, out)
	}
}

// TestReconcileIgnoredWithoutMergedFact pins the reconciliation boundary: an
// item that never merged (no durable fact) is left alone, so reconciliation can
// never close work that merely has no branch.
func TestReconcileIgnoredWithoutMergedFact(t *testing.T) {
	work := setupTestRepo(t)
	st := &closeTracker{items: []Item{{ID: "9", Title: "change"}}}
	old := trackerFor
	trackerFor = func(string) Tracker { return st }
	defer func() { trackerFor = old }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"

	if err := reconcileMerged(cfg, work); err != nil {
		t.Fatalf("reconcileMerged: %v", err)
	}
	if len(st.closed) != 0 {
		t.Fatalf("reconciliation closed an item with no merged fact: %v", st.closed)
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatal(err)
	}
}
