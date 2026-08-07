package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestProjectionDisabledPerformsNoHostCall(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	calls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		calls++
		return nil, errors.New("host call")
	}

	cfg := Config{Repo: "owner/repo"}
	if got, err := projectBranch(cfg, Item{ID: "7", Title: "change"}, "forest/7-change", "body"); err != nil || got != "" {
		t.Fatalf("disabled projectBranch = (%q, %v), want (empty, nil)", got, err)
	}
	if err := projectVerdict(cfg, "forest/7-change", verdictNote{Verdict: "approve"}, checksNote{Status: "pass"}); err != nil {
		t.Fatalf("disabled projectVerdict: %v", err)
	}
	if calls != 0 {
		t.Fatalf("disabled projection made %d host calls", calls)
	}
}

func TestProjectBranchReusesExistingOpenRequest(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	calls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		calls++
		return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23"}]`), nil
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	got, err := projectBranch(cfg, Item{Title: "change"}, "forest/7-change", "body")
	if err != nil {
		t.Fatalf("projectBranch: %v", err)
	}
	if got != "https://github.com/owner/repo/pull/23" {
		t.Fatalf("projectBranch URL = %q", got)
	}
	if calls != 1 {
		t.Fatalf("existing request caused %d host calls, want one list call", calls)
	}
}

func TestProjectVerdictCommentContainsDecisionAndChecks(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var commentArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch args[1] {
		case "list":
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23"}]`), nil
		case "comment":
			commentArgs = append([]string(nil), args...)
			return nil, nil
		default:
			return nil, errors.New("unexpected host command")
		}
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	err := projectVerdict(cfg, "forest/7-change", verdictNote{Verdict: "changes", Notes: "repair the parser"}, checksNote{
		Status:  "fail",
		Results: []checkResult{{Name: "test", Code: 1, Seconds: 2.5, Output: "assertion failed"}},
	})
	if err != nil {
		t.Fatalf("projectVerdict: %v", err)
	}
	body := strings.Join(commentArgs, "\n")
	for _, want := range []string{"changes", "repair the parser", "fail", "test", "assertion failed"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment body missing %q: %s", want, body)
		}
	}
}

func TestProjectVerdictMissingRequestIsNoop(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	comments := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[1] == "comment" {
			comments++
		}
		return []byte(`[]`), nil
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if err := projectVerdict(cfg, "forest/7-change", verdictNote{Verdict: "approve"}, checksNote{Status: "pass"}); err != nil {
		t.Fatalf("missing pull request: %v", err)
	}
	if comments != 0 {
		t.Fatalf("missing pull request received %d comments", comments)
	}
}

func TestProjectMergeDisabledReturnsError(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	calls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		calls++
		return nil, nil
	}

	err := projectMerge(Config{Repo: "owner/repo"}, "forest/7-change", "squash", "abc123")
	if err == nil {
		t.Fatal("disabled projectMerge returned nil")
	}
	if calls != 0 {
		t.Fatalf("disabled projectMerge made %d host calls", calls)
	}
}

// TestProjectMergeRefusesMovedProjectionHead is the host-path counterpart to
// TestMergeVerifiedRefusesMovedBranch. The git-side expected-head check closes
// the window up to the merge call, but when the host owns the target branch the
// merge is issued against a pull request listed by name: a push to the forest
// branch after admission would otherwise make the host land an unchecked commit.
// projectMerge must match the projection's head against the admitted revision
// before running pr merge, refusing when a push advanced it.
func TestProjectMergeRefusesMovedProjectionHead(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var called []string
	projectionCommand = func(args ...string) ([]byte, error) {
		called = append(called, args[1])
		return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"deadbeef"}]`), nil
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	err := projectMerge(cfg, "forest/7-change", "squash", "cafebabe")
	if err == nil {
		t.Fatal("projectMerge landed a projection whose head moved after admission")
	}
	for _, c := range called {
		if c == "merge" {
			t.Fatal("projectMerge issued pr merge for a moved projection head")
		}
	}
}

// TestProjectMergeMatchesAdmittedHeadAndCAS pins the happy path of the host
// compare-and-swap: a projection still pointing at the exact admitted revision
// is merged exactly once, and the merge call itself carries the provider-side
// --match-head-commit so a push between the list and the merge cannot land an
// unchecked head.
func TestProjectMergeMatchesAdmittedHeadAndCAS(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	merges := 0
	var mergeArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch args[1] {
		case "list":
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"cafebabe"}]`), nil
		case "merge":
			merges++
			mergeArgs = append([]string(nil), args...)
			return nil, nil
		default:
			return nil, errors.New("unexpected host command")
		}
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if err := projectMerge(cfg, "forest/7-change", "squash", "cafebabe"); err != nil {
		t.Fatalf("projectMerge refused a matching admitted head: %v", err)
	}
	if merges != 1 {
		t.Fatalf("projectMerge made %d merge calls, want 1", merges)
	}
	if !reflect.DeepEqual(mergeArgs[1:], []string{"merge", "23", "-R", "owner/repo", "--squash", "--match-head-commit", "cafebabe"}) {
		t.Fatalf("merge call args = %q, want the host CAS --match-head-commit", mergeArgs[1:])
	}
}

// TestProjectMergeRefusesEmptyProjectionHead pins that an empty reported head
// is not an admission the machine can trust: without a real headRefOid there is
// no way to compare-and-swap, so the merge is refused rather than let an
// unreported head land.
func TestProjectMergeRefusesEmptyProjectionHead(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var called []string
	projectionCommand = func(args ...string) ([]byte, error) {
		called = append(called, args[1])
		return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23"}]`), nil
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	err := projectMerge(cfg, "forest/7-change", "squash", "cafebabe")
	if err == nil {
		t.Fatal("projectMerge accepted a projection with no reported head")
	}
	for _, c := range called {
		if c == "merge" {
			t.Fatal("projectMerge issued pr merge for a projection with no reported head")
		}
	}
}

func TestPRNumberFromURL(t *testing.T) {
	if got := prNumberFromURL("https://github.com/owner/repo/pull/23"); got != 23 {
		t.Fatalf("prNumberFromURL = %d, want 23", got)
	}
	for _, malformed := range []string{"", "not-a-url", "https://github.com/owner/repo/pull/", "https://github.com/owner/repo/pull/nope"} {
		if got := prNumberFromURL(malformed); got != 0 {
			t.Errorf("prNumberFromURL(%q) = %d, want 0", malformed, got)
		}
	}
}
