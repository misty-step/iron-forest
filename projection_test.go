package main

import (
	"errors"
	"strings"
	"testing"
)

func hasArgumentPair(args []string, key, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == key && args[i+1] == value {
			return true
		}
	}
	return false
}

func mergedProjectionPage(body string) []byte {
	return []byte("[[" + body + "]]")
}

func TestProjectionDisabledPerformsNoHostCall(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	calls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		calls++
		return nil, errors.New("host call")
	}

	cfg := Config{Repo: "owner/repo"}
	if got, _, err := projectBranch(cfg, Item{ID: "7", Title: "change"}, "forest/7-change", "body", ""); err != nil || got != "" {
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
	var listArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		calls++
		listArgs = append([]string(nil), args...)
		return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	got, _, err := projectBranch(cfg, Item{Title: "change"}, "forest/7-change", "body", "")
	if err != nil {
		t.Fatalf("projectBranch: %v", err)
	}
	if got != "https://github.com/owner/repo/pull/23" {
		t.Fatalf("projectBranch URL = %q", got)
	}
	if calls != 1 {
		t.Fatalf("existing request caused %d host calls, want one list call", calls)
	}
	if hasArgumentPair(listArgs, "--base", "master") {

		t.Fatalf("projection list args %v hide requests to another target branch", listArgs)
	}
	if !hasArgumentPair(listArgs, "--head", "forest/7-change") {
		t.Fatalf("projection list args %v do not constrain the source branch", listArgs)
	}
}
func TestProjectBranchRecognizesAlreadyMergedReviewedHead(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	const reviewed = "0123456789abcdef0123456789abcdef01234567"
	createCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		case args[0] == "api":
			return []byte(`[[{"number":22,"html_url":"https://github.com/owner/repo/pull/22","merged_at":"2026-08-07T00:00:00Z","head":{"sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ref":"forest/7-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}],[{"number":23,"html_url":"https://github.com/owner/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"forest/7-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}]]`), nil
		case args[0] == "pr" && args[1] == "create":
			createCalls++
			return nil, errors.New("duplicate create")
		default:
			return nil, errors.New("unexpected host command")
		}
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true, MergeViaHost: true}}
	url, merged, err := projectBranch(cfg, Item{Title: "change"}, "forest/7-change", "body", reviewed)
	if err != nil || !merged || url != "https://github.com/owner/repo/pull/23" {
		t.Fatalf("merged Projection recovery = (%q, %v, %v)", url, merged, err)
	}
	cfg.Projection.MergeViaHost = false
	url, merged, err = projectBranch(cfg, Item{Title: "change"}, "forest/7-change", "body", reviewed)
	if err != nil || merged || url != "https://github.com/owner/repo/pull/23" {
		t.Fatalf("one-way Projection recovery = (%q, %v, %v)", url, merged, err)
	}
	if createCalls != 0 {
		t.Fatalf("merged Projection recovery created %d duplicate request(s)", createCalls)
	}
}

func TestProjectBranchCreatesMissingRequest(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var createArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch args[1] {
		case "list":
			return []byte(`[]`), nil
		case "create":
			createArgs = append([]string(nil), args...)
			return []byte("https://github.com/owner/repo/pull/24\n"), nil
		default:
			return nil, errors.New("unexpected host command")
		}
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	const secret = "AKIA1234567890ABCDEF"
	got, _, err := projectBranch(cfg, Item{Title: "change " + secret},
		"forest/7-change", "body "+secret, "")
	if err != nil || got != "https://github.com/owner/repo/pull/24" {
		t.Fatalf("projectBranch create = (%q, %v)", got, err)
	}
	if !hasArgumentPair(createArgs, "--base", "master") ||
		!hasArgumentPair(createArgs, "--head", "forest/7-change") {
		t.Fatalf("projection create args = %v, want master target and exact source", createArgs)
	}
	if !hasArgumentPair(createArgs, "--title", "forest: change "+secretRedacted) ||
		!hasArgumentPair(createArgs, "--body", "body "+secretRedacted) {
		t.Fatalf("projection create args retained mutable secret-shaped text: %v", createArgs)
	}
}

func TestProjectVerdictCommentContainsDecisionAndChecks(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var commentArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch args[1] {
		case "list":
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
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
func TestProjectChecksRedactsSecretShapedCheckName(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var commentArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		switch args[1] {
		case "list":
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
		case "comment":
			commentArgs = append([]string(nil), args...)
			return nil, nil
		default:
			return nil, errors.New("unexpected host command")
		}
	}

	const secret = "sk-AAAAAAAAAAAAAAAA"
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	err := projectChecks(cfg, "forest/7-change", checksNote{
		Status:  "fail",
		Results: []checkResult{{Name: "lint-" + secret, Code: 1, Output: "failed"}},
	})
	if err != nil {
		t.Fatalf("projectChecks: %v", err)
	}
	body := strings.Join(commentArgs, "\n")
	if strings.Contains(body, secret) || !strings.Contains(body, secretRedacted) {
		t.Fatalf("Host check comment redaction = %q, want marker without original", body)
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

func TestProjectionRejectsForeignSource(t *testing.T) {
	tests := map[string]struct {
		open   string
		merged string
	}{
		"cross repository": {
			open:   `[{"number":23,"url":"https://github.com/fork/repo/pull/23","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":true}]`,
			merged: `[[{"number":23,"html_url":"https://github.com/fork/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"0123456789abcdef0123456789abcdef01234567","ref":"forest/7-change","repo":{"full_name":"fork/repo"}},"base":{"ref":"master"}}]]`,
		},
		"wrong branch": {
			open:   `[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefName":"forest/8-change","baseRefName":"master","isCrossRepository":false}]`,
			merged: `[[{"number":23,"html_url":"https://github.com/owner/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"0123456789abcdef0123456789abcdef01234567","ref":"forest/8-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}]]`,
		},
		"wrong target": {
			open:   `[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefName":"forest/7-change","baseRefName":"release","isCrossRepository":false}]`,
			merged: `[[{"number":23,"html_url":"https://github.com/owner/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"0123456789abcdef0123456789abcdef01234567","ref":"forest/7-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"release"}}]]`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			old := projectionCommand
			defer func() { projectionCommand = old }()
			projectionCommand = func(args ...string) ([]byte, error) {
				if args[0] == "api" {
					return []byte(tc.merged), nil
				}
				return []byte(tc.open), nil
			}
			cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
			if _, err := projectionPRs(cfg, "forest/7-change"); err == nil ||
				!strings.Contains(err.Error(), "does not originate") {
				t.Fatalf("open foreign request = %v, want refusal", err)
			}
			if _, err := mergedProjectionPRs(cfg, "forest/7-change"); err == nil ||
				!strings.Contains(err.Error(), "does not originate") {
				t.Fatalf("merged foreign request = %v, want refusal", err)
			}
		})
	}
}

func TestOpenProjectionRejectsMultipleRequests(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	projectionCommand = func(args ...string) ([]byte, error) {
		return []byte(`[
			{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false},
			{"number":24,"url":"https://github.com/owner/repo/pull/24","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}
		]`), nil
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if _, err := openProjectionPR(cfg, "forest/7-change"); err == nil ||
		!strings.Contains(err.Error(), "multiple open") {
		t.Fatalf("ambiguous open requests = %v, want refusal", err)
	}
}

func TestMergedProjectionUsesPaginatedExactAPIQuery(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var got []string
	projectionCommand = func(args ...string) ([]byte, error) {
		got = append([]string(nil), args...)
		return []byte(`[]`), nil
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if _, err := mergedProjectionPRs(cfg, "forest/7-change"); err != nil {
		t.Fatalf("merged Projection lookup: %v", err)
	}
	if len(got) == 0 || got[0] != "api" {
		t.Fatalf("merged Projection command = %v, want gh api", got)
	}
	for _, flag := range []string{"--paginate", "--slurp"} {
		found := false
		for _, arg := range got {
			if arg == flag {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("merged Projection command = %v, missing %s", got, flag)
		}
	}
	for _, arg := range got {
		if arg == "--limit" {
			t.Fatalf("merged Projection command reintroduced fixed gh pr list limit: %v", got)
		}
	}
	if !hasArgumentPair(got, "--method", "GET") ||
		!hasArgumentPair(got, "--field", "state=closed") ||
		!hasArgumentPair(got, "--field", "head=owner:forest/7-change") ||
		!hasArgumentPair(got, "--field", "base=master") ||
		!hasArgumentPair(got, "--field", "per_page=100") {
		t.Fatalf("merged Projection command = %v, missing exact paginated query", got)
	}
	endpoint := false
	for _, arg := range got {
		if arg == "repos/owner/repo/pulls" {
			endpoint = true
			break
		}
	}
	if !endpoint {
		t.Fatalf("merged Projection command = %v, missing exact repository endpoint", got)
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

	err := projectMerge(Config{Repo: "owner/repo"}, "forest/7-change", "squash", "")
	if err == nil {
		t.Fatal("disabled projectMerge returned nil")
	}
	if calls != 0 {
		t.Fatalf("disabled projectMerge made %d host calls", calls)
	}
}

// TestProjectMergePinsExpectedHead pins item #188's host path: the reviewed
// Revision is passed to the host's expected-head facility so a host merge never
// acts on a head that advanced past the Verdict, while an empty expected head
// leaves the merge unpinned.
func TestProjectMergePinsExpectedHead(t *testing.T) {
	run := func(t *testing.T, expectedHead string) []string {
		t.Helper()
		old := projectionCommand
		defer func() { projectionCommand = old }()
		var mergeArgs []string
		merged := false
		projectionCommand = func(args ...string) ([]byte, error) {
			switch {
			case args[0] == "api" && merged:
				return mergedProjectionPage(`{"number":23,"html_url":"https://github.com/owner/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + expectedHead + `","ref":"forest/7-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
			case args[0] == "api":
				return []byte(`[]`), nil
			case args[0] == "pr" && args[1] == "list":
				return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` + expectedHead + `","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
			case args[0] == "pr" && args[1] == "merge":
				mergeArgs = append([]string(nil), args...)
				merged = true
				return nil, nil
			default:
				return nil, errors.New("unexpected host command")
			}
		}
		cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
		if err := projectMerge(cfg, "forest/7-change", "squash", expectedHead); err != nil {
			t.Fatalf("projectMerge: %v", err)
		}
		return mergeArgs
	}

	const reviewed = "0123456789abcdef0123456789abcdef01234567"
	args := run(t, reviewed)
	found := false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--match-head-commit" && args[i+1] == reviewed {
			found = true
		}
	}
	if !found {
		t.Fatalf("projectMerge args %v do not pin the reviewed head %s", args, reviewed)
	}

	for _, a := range run(t, "") {
		if a == "--match-head-commit" {
			t.Fatalf("projectMerge pinned a merge with an empty expected head: %v", args)
		}
	}
}

func TestProjectMergeRecoversAnAlreadyMergedReviewedHead(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	const reviewed = "0123456789abcdef0123456789abcdef01234567"
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		case args[0] == "api":
			return mergedProjectionPage(`{"number":23,"html_url":"https://github.com/owner/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"forest/7-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
			mergeCalls++
			return nil, nil
		default:
			return nil, errors.New("unexpected host command")
		}
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if err := projectMerge(cfg, "forest/7-change", "squash", reviewed); err != nil {
		t.Fatalf("recover merged request: %v", err)
	}
	if mergeCalls != 0 {
		t.Fatalf("recovery asked a merged request to merge %d time(s)", mergeCalls)
	}
	if err := projectMerge(cfg, "forest/7-change", "squash", strings.Repeat("f", 40)); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched merged head = %v, want refusal", err)
	}
}
func TestProjectMergeWaitsForQueuedHostConfirmation(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	const reviewed = "0123456789abcdef0123456789abcdef01234567"
	hostMerged := false
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "api" && hostMerged:
			return mergedProjectionPage(`{"number":23,"html_url":"https://github.com/owner/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"forest/7-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case args[0] == "api":
			return []byte(`[]`), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":23,"headRefOid":"` + reviewed + `","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "pr" && args[1] == "merge":
			mergeCalls++
			hostMerged = true
			return nil, nil
		default:
			return nil, errors.New("unexpected host command")
		}
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if err := projectMerge(cfg, "forest/7-change", "squash", reviewed); !errors.Is(err, errHostMergePending) {
		t.Fatalf("queued Host merge = %v, want pending", err)
	}
	hostMerged = true
	if err := projectMerge(cfg, "forest/7-change", "squash", reviewed); err != nil {
		t.Fatalf("confirmed Host merge recovery: %v", err)
	}
	if mergeCalls != 1 {
		t.Fatalf("Host merge calls = %d, want 1", mergeCalls)
	}
}

func TestProjectMergeRejectsHostFastForward(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	calls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		calls++
		return nil, nil
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if err := projectMerge(cfg, "forest/7-change", "ff", strings.Repeat("a", 40)); err == nil ||
		!strings.Contains(err.Error(), "only squash") {
		t.Fatalf("Host fast-forward merge = %v, want refusal", err)
	}
	if calls != 0 {
		t.Fatalf("Host fast-forward refusal made %d Host calls", calls)
	}
}
