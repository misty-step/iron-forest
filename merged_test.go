package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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

// TestCloseIsIdempotent pins the Tracker contract change of item #190: closing
// an item that is already closed returns nil. Without this a crash after Close
// but before a subject's branch or attempt cleanup would make a later
// reconciliation pass fail forever on the already-closed item, blocking the pass
// from finishing those effects.
func TestCloseIsIdempotent(t *testing.T) {
	m := newMemoryTracker()
	m.seed(Item{ID: "9", Title: "change"})
	if err := m.Close("9"); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close("9"); err != nil {
		t.Fatalf("second Close on an already-closed item: %v", err)
	}
}

// TestHostMergeFactIsDurableBeforeClose proves the reviewer's ordering point for
// item #190: on the host path the durable merged fact is written before the merge
// is committed to, so even when Close is then forced to fail, the fact survives
// and startup reconciliation finds it. The Builder must not select the still-open
// item the moment the fact is durable, and a later pass completes the close.
func TestHostMergeFactIsDurableBeforeClose(t *testing.T) {
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

	oldProj := projectionCommand
	defer func() { projectionCommand = oldProj }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "pr" && len(args) > 1 && args[1] == "merge" {
			return nil, nil // the host lands the merge
		}
		switch args[1] {
		case "list":
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9"}]`), nil
		default:
			return nil, nil
		}
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"

	it := Item{ID: "9", Title: "change"}
	// Close fails after the merge lands; the fact must already be durable.
	if err := mergeVerified(cfg, repo, branch, reviewed, it); err == nil {
		t.Fatal("mergeVerified with Close failing returned no error")
	}
	set, err := mergedIDs(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !set["9"] {
		t.Fatalf("host merge left no durable fact when Close failed: %v", set)
	}
	// The still-open, now-branchless item must not be eligible for the Builder.
	if items, err := eligibleItems(cfg, repo); err != nil || len(items) != 0 {
		t.Fatalf("merged-but-unclosed item eligible after failed close: %+v, err=%v", items, err)
	}
	// A later pass reconciles: the outage clears and finalisation completes.
	st.closeErr = nil
	if err := reconcileMerged(cfg, repo); err != nil {
		t.Fatalf("reconcileMerged: %v", err)
	}
	if len(st.closed) < 2 {
		t.Fatalf("item closed %v times, want a failed attempt plus a reconciliation close", st.closed)
	}
}

// TestVerifierOffersBranchAdvancedPastMergedRevision pins the reviewer's third
// point for item #190: a durable fact suppresses only the exact Revision it
// recorded. A branch still pointing at that merged Revision is never offered for
// a second merge; a branch that advanced past it is new, unreviewed work and is
// offered as fresh rather than stranded by an item-wide fact.
func TestVerifierOffersBranchAdvancedPastMergedRevision(t *testing.T) {
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

	// No verdict yet: the merged Revision branch is fresh work until we mark it.
	if err := markMerged(repo, "9", mergedNote{Branch: branch, Revision: reviewed}); err != nil {
		t.Fatalf("markMerged: %v", err)
	}
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range subjects {
		if s.Branch == branch {
			t.Fatalf("verifier offered a branch still at the merged Revision: %#v", s)
		}
	}

	// The branch advances past the merged Revision with newer, unreviewed work.
	rebaseTestGit(t, repo, "checkout", "-q", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
	rebaseTestGit(t, repo, "add", "later.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "newer unreviewed work")
	rebaseTestGit(t, repo, "push", "-q", "origin", branch)
	newHead := remoteBranchHead(t, repo, branch)
	if newHead == reviewed {
		t.Fatal("branch did not advance, cannot probe revision-scoped suppression")
	}

	subjects, err = (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	var offered bool
	for _, s := range subjects {
		if s.Branch == branch {
			offered = true
			if s.Head != newHead {
				t.Fatalf("offered branch head = %s, want the newer head %s", s.Head, newHead)
			}
		}
	}
	if !offered {
		t.Fatalf("verifier did not offer the advanced branch as fresh work: %#v", subjects)
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

// TestPendingClaimRolledBackWhenMergeNeverLanded proves the reviewer's first
// point for item #190: a durable pending claim is not treated as proof that a
// merge landed. When the host confirms the pull request never merged, reconciliation
// rolls the premature claim back and leaves the item and branch as fresh work —
// it does not close the item or delete the branch.
func TestPendingClaimRolledBackWhenMergeNeverLanded(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	st := &closeTracker{items: []Item{{ID: "9", Title: "change"}}}
	old := trackerFor
	trackerFor = func(string) Tracker { return st }
	defer func() { trackerFor = old }()

	oldProj := projectionCommand
	defer func() { projectionCommand = oldProj }()
	projectionCommand = func(args ...string) ([]byte, error) {
		return []byte(`{"state":"open"}`), nil // the host never merged it
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true}

	// A crash after committing to the merge but before the merge ran leaves a
	// pending claim (the equivalent of a crash right after the claim write).
	if err := markMergedClaim(repo, "9", mergedNote{Branch: branch, Revision: reviewed}); err != nil {
		t.Fatalf("markMergedClaim: %v", err)
	}
	if err := reconcileMerged(cfg, repo); err != nil {
		t.Fatalf("reconcileMerged: %v", err)
	}
	if len(st.closed) != 0 {
		t.Fatalf("never-merged item was closed: %v", st.closed)
	}
	if out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out == "" {
		t.Fatalf("never-merged branch %q was deleted by reconciliation", branch)
	}
	set, err := mergedIDs(repo)
	if err != nil {
		t.Fatal(err)
	}
	if set["9"] {
		t.Fatal("pending claim not rolled back after the merge was confirmed absent")
	}
}

// TestPendingClaimFinalisedWhenHostMerged proves the complementary half of the
// host-merge protocol: when the host did merge the pull request (a crash or an
// ambiguous network error after the merge landed but before the claim graduated),
// reconciliation observes that, graduates the claim to landed, and completes the
// finalisation — it never erases the proof of a real merge.
func TestPendingClaimFinalisedWhenHostMerged(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	st := &closeTracker{items: []Item{{ID: "9", Title: "change"}}}
	old := trackerFor
	trackerFor = func(string) Tracker { return st }
	defer func() { trackerFor = old }()

	oldProj := projectionCommand
	defer func() { projectionCommand = oldProj }()
	projectionCommand = func(args ...string) ([]byte, error) {
		return []byte(`{"state":"merged"}`), nil // the host did merge it
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true}

	if err := markMergedClaim(repo, "9", mergedNote{Branch: branch, Revision: reviewed}); err != nil {
		t.Fatalf("markMergedClaim: %v", err)
	}
	if err := reconcileMerged(cfg, repo); err != nil {
		t.Fatalf("reconcileMerged: %v", err)
	}
	if len(st.closed) != 1 || st.closed[0] != "9" {
		t.Fatalf("merged item closed = %v, want [9]", st.closed)
	}
	if out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
		t.Fatalf("landed branch %q still on origin after reconciliation: %s", branch, out)
	}
	set, err := mergedIDs(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !set["9"] {
		t.Fatal("landed claim was dropped instead of being retained")
	}
}

// TestProjectMergeErrorLeavesClaimForReconciliation proves the reviewer's second
// point for item #190: an ambiguous host/network error on the merge does not drop
// the durable claim. Dropping it could erase proof that the merge actually landed;
// instead the claim survives for reconciliation to observe. Until then the item is
// never closed and its branch is never deleted.
func TestProjectMergeErrorLeavesClaimForReconciliation(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	st := &closeTracker{items: []Item{{ID: "9", Title: "change"}}}
	old := trackerFor
	trackerFor = func(string) Tracker { return st }
	defer func() { trackerFor = old }()

	oldProj := projectionCommand
	defer func() { projectionCommand = oldProj }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "merge" {
			return nil, errors.New("ambiguous host/network error")
		}
		if len(args) > 1 && args[1] == "list" {
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9"}]`), nil
		}
		return []byte(`{"state":"merged"}`), nil // the merge actually landed
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"

	if err := mergeVerified(cfg, repo, branch, reviewed, Item{ID: "9", Title: "change"}); err == nil {
		t.Fatal("mergeVerified with an ambiguous merge error returned nil")
	}
	set, err := mergedIDs(repo)
	if err != nil {
		t.Fatal(err)
	}
	if !set["9"] {
		t.Fatal("durable claim erased after an ambiguous merge error")
	}
	if len(st.closed) != 0 {
		t.Fatalf("item closed while the merge was unconfirmed: %v", st.closed)
	}
	// Reconciliation observes the host and completes the finalisation.
	if err := reconcileMerged(cfg, repo); err != nil {
		t.Fatalf("reconcileMerged: %v", err)
	}
	if len(st.closed) != 1 {
		t.Fatalf("item closed = %v, want one close after reconcile", st.closed)
	}
	if out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
		t.Fatalf("landed branch %q not deleted after reconcile: %s", branch, out)
	}
}

// TestMergeHostMergeSerializesWithReconcileOverPendingClaim covers the reviewer's
// interleaving for item #190: reconcileMerged runs concurrently with the verifier's
// in-flight host merge and did not serialize with it. On the flawed path, between
// markMergedClaim and projectMerge reconciliation would observe the pull request
// as open, drop the claim, and then let the merge succeed with no claim left to
// confirm, so confirmMerged failed with "no claim recorded" and finishMerge never
// ran. mergeHostPath now holds mergeCoord across the claim→merge→confirm sequence,
// and reconcileMerged holds the same lock per subject, so the rollback can never
// race the in-flight merge: the merge always completes and the item is closed.
func TestMergeHostMergeSerializesWithReconcileOverPendingClaim(t *testing.T) {
	repo := setupTestRepo(t)
	branch := "forest/9-change"
	rebaseTestGit(t, repo, "checkout", "-q", "-b", branch)
	rebaseTestWriteFile(t, filepath.Join(repo, "branch.txt"), "branch\n")
	rebaseTestGit(t, repo, "add", "branch.txt")
	rebaseTestGit(t, repo, "commit", "-q", "-m", "branch work")
	rebaseTestGit(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/"+branch)
	reviewed := remoteBranchHead(t, repo, branch)
	rebaseTestGit(t, repo, "checkout", "-q", "master")

	st := &closeTracker{items: []Item{{ID: "9", Title: "change"}}}
	old := trackerFor
	trackerFor = func(string) Tracker { return st }
	defer func() { trackerFor = old }()

	mergeStarted := make(chan struct{})
	releaseMerge := make(chan struct{})
	sawOpen := make(chan struct{}, 1)

	oldProj := projectionCommand
	defer func() { projectionCommand = oldProj }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) > 1 && args[1] == "list" {
			return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9"}]`), nil
		}
		if len(args) > 1 && args[1] == "view" {
			select {
			case sawOpen <- struct{}{}:
			default:
			}
			return []byte(`{"state":"open"}`), nil // observe the PR before the merge
		}
		if len(args) > 1 && args[1] == "merge" {
			select {
			case mergeStarted <- struct{}{}:
			default:
			}
			<-releaseMerge  // park the merge mid-flight
			return nil, nil // the host merges
		}
		return nil, nil
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"
	it := Item{ID: "9", Title: "change"}

	// A crash before a previous merge ran left a pending claim, so the claim
	// already exists when the resumed in-flight merge re-derives the world.
	if err := markMergedClaim(repo, "9", mergedNote{Branch: branch, Revision: reviewed}); err != nil {
		t.Fatalf("markMergedClaim: %v", err)
	}

	var mergeErr, recErr error
	mergeDone := make(chan struct{})
	go func() {
		mergeErr = mergeVerified(cfg, repo, branch, reviewed, it)
		close(mergeDone)
	}()
	<-mergeStarted // the merge is committed to and mid-flight

	reconcileDone := make(chan struct{})
	go func() {
		recErr = reconcileMerged(cfg, repo)
		close(reconcileDone)
	}()

	// Give reconciliation a moment to try to observe the claim. On a serialized
	// implementation it blocks on mergeCoord and must not see the claim as open;
	// on the flawed path it drops the claim, which makes the bug observable.
	select {
	case <-sawOpen:
	case <-time.After(300 * time.Millisecond):
	}
	close(releaseMerge)

	<-mergeDone
	<-reconcileDone

	if mergeErr != nil {
		t.Fatalf("mergeVerified: %v", mergeErr)
	}
	if recErr != nil {
		t.Fatalf("reconcileMerged: %v", recErr)
	}
	if len(st.closed) == 0 || st.closed[len(st.closed)-1] != "9" {
		t.Fatalf("merged item not closed after merge: %v", st.closed)
	}
	note, ok, err := mergedNoteFor(repo, "9")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || !note.landed() {
		t.Fatalf("merged fact not landed after merge: %+v ok=%v", note, ok)
	}
	if out := rebaseTestGitOut(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
		t.Fatalf("merged branch %q still present: %s", branch, out)
	}
	if len(sawOpen) != 0 {
		t.Fatalf("reconciliation observed the claim open mid-merge: the rollback raced the in-flight merge")
	}
}

// TestMarkMergedConcurrentIsIdempotent pins the reviewer's point for item #190
// that markMerged must not surface CAS errors on concurrent retries despite being
// described as idempotent. Many passes racing the same compare-and-set all return
// nil and exactly one durable fact is recorded.
func TestMarkMergedConcurrentIsIdempotent(t *testing.T) {
	repo := setupTestRepo(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := markMerged(repo, "9", mergedNote{Branch: "forest/9-change", Revision: "deadbeef"}); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		t.Errorf("markMerged concurrent: %v", err)
	}
	refs, err := listRefs(repo, mergedRefPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("merged refs = %d, want exactly 1 durable fact", len(refs))
	}
}

// TestDropMergedClaimConcurrentIsIdempotent pins the companion point for item
// #190: dropMergedClaim must not surface CAS errors when many passes roll a pending
// claim back at once. All return nil, and the claim is gone afterwards.
func TestDropMergedClaimConcurrentIsIdempotent(t *testing.T) {
	repo := setupTestRepo(t)
	if err := markMergedClaim(repo, "9", mergedNote{Branch: "forest/9-change", Revision: "deadbeef"}); err != nil {
		t.Fatalf("markMergedClaim: %v", err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := dropMergedClaim(repo, "9"); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("dropMergedClaim: %w", err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		t.Errorf("%v", err)
	}
	set, err := mergedIDs(repo)
	if err != nil {
		t.Fatal(err)
	}
	if set["9"] {
		t.Fatal("pending claim not dropped after concurrent rollback")
	}
}
