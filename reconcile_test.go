package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestDetectOrphansPinsOracle locks the reconcile contract from issue #49: the
// pass must list PRs #4, #5, #6, and #8 as orphaned — each with its originating
// closed issue id and a reason — while a merged PR and a PR whose issue is
// still open are left alone.
func TestDetectOrphansPinsOracle(t *testing.T) {
	workspace := t.TempDir()
	lines := []string{
		`{"time":"2026-08-05T00:00:00Z","pr":4,"branch":"forest/1","issue":1,"state":"opened"}`,
		`{"time":"2026-08-05T00:00:00Z","pr":5,"branch":"forest/2","issue":2,"state":"stalled"}`,
		`{"time":"2026-08-05T00:00:00Z","pr":6,"branch":"forest/3","issue":3,"state":"closed"}`,
		// #7 merged its change, so it must never be flagged.
		`{"time":"2026-08-05T00:00:00Z","pr":7,"branch":"forest/5","issue":5,"state":"merged"}`,
		`{"time":"2026-08-05T00:00:00Z","pr":8,"branch":"forest/7","issue":7,"state":"ready"}`,
		// #9 is unmerged but its issue is still open: in flight, not orphaned.
		`{"time":"2026-08-05T00:00:00Z","pr":9,"branch":"forest/8","issue":8,"state":"opened"}`,
	}
	if err := os.WriteFile(filepath.Join(workspace, "prs.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Issues #1/#2/#3/#7 closed when their PRs opened; #8 stays open. No PR is
	// live-merged.
	closed := map[int]bool{1: true, 2: true, 3: true, 7: true}
	got, err := detectOrphans(workspace,
		func(n int) (bool, error) { return false, nil },
		func(n int) (bool, error) { return closed[n], nil })
	if err != nil {
		t.Fatalf("detectOrphans: %v", err)
	}

	want := []orphan{
		{PR: 4, Branch: "forest/1", Issue: 1, Reason: "issue #1 closed but PR #4 still open"},
		{PR: 5, Branch: "forest/2", Issue: 2, Reason: "issue #2 closed; PR #5 stalled and unmerged"},
		{PR: 6, Branch: "forest/3", Issue: 3, Reason: "issue #3 closed; PR #6 closed without merging"},
		{PR: 8, Branch: "forest/7", Issue: 7, Reason: "issue #7 closed but PR #8 still open"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orphans mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

// TestDetectOrphansEmptyLedger pins that a workspace with no PR ledger (or an
// unreadable one) is a valid state: zero orphans and no error, never a crash.
func TestDetectOrphansEmptyLedger(t *testing.T) {
	got, err := detectOrphans(t.TempDir(),
		func(int) (bool, error) { return false, nil },
		func(int) (bool, error) { return false, nil })
	if err != nil {
		t.Fatalf("detectOrphans on empty ledger: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty ledger yielded orphans: %+v", got)
	}
}

// TestClosedPRRowPreservesIssue pins the lifecycle watchPR writes when a PR is
// no longer open: the final row must carry the originating issue forward from
// the last known row. Without this the latest row loses attribution, so
// latestPRStates keeps an Issue=0 row and detectOrphans silently skips a
// closed, unmerged PR — a closed orphan would never be reported.
func TestClosedPRRowPreservesIssue(t *testing.T) {
	base := prState{Time: "2026-08-05T00:00:00Z", PR: 6, PRURL: "https://example.test/pr/6", Branch: "forest/3"}
	last := prState{PR: 6, Branch: "forest/3", Issue: 3, State: "opened"}

	row := closedPRRow(base, last)
	if row.State != "closed" {
		t.Fatalf("state = %q, want closed", row.State)
	}
	if row.Issue != 3 {
		t.Fatalf("issue = %d, want 3 (the closed row must keep its attribution)", row.Issue)
	}
	if row.PR != 6 || row.Branch != "forest/3" {
		t.Fatalf("row = %+v, must carry the PR identity", row)
	}
}

// TestDetectOrphansMergedIsAuthoritative pins the live merge check: a PR whose
// last ledger row is opened/ready/stalled must not be reported orphaned when
// its current live state is merged. A merge landing after the ledger's final
// row — manual, or through a path the loop did not record — still clears it.
func TestDetectOrphansMergedIsAuthoritative(t *testing.T) {
	workspace := t.TempDir()
	lines := []string{
		`{"time":"2026-08-05T00:00:00Z","pr":11,"branch":"forest/11","issue":11,"state":"opened"}`,
		`{"time":"2026-08-05T00:00:00Z","pr":12,"branch":"forest/12","issue":12,"state":"stalled"}`,
	}
	if err := os.WriteFile(filepath.Join(workspace, "prs.jsonl"),
		[]byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// liveMerged: #11 merged after its opened row; #12 is still open.
	merged := map[int]bool{11: true}
	got, err := detectOrphans(workspace,
		func(n int) (bool, error) { return merged[n], nil },
		func(n int) (bool, error) { return true, nil }) // every issue closed
	if err != nil {
		t.Fatalf("detectOrphans: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d orphans, want 1: %+v", len(got), got)
	}
	if got[0].PR != 12 {
		t.Fatalf("orphan PR = %d, want 12 (only the live-unmerged PR remains)", got[0].PR)
	}
}
