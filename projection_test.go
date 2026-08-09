package main

import (
	"encoding/json"
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

const projectionTestHead = "0123456789abcdef0123456789abcdef01234567"

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
	if got, _, err := projectBranch(cfg, "", Item{ID: "7", Title: "change"}, "forest/7-change", "body", ""); err != nil || got != "" {
		t.Fatalf("disabled projectBranch = (%q, %v), want (empty, nil)", got, err)
	}
	if err := projectVerdict(cfg, "forest/7-change", projectionTestHead, verdictNote{Verdict: "approve"}, checksNote{Status: "pass"}); err != nil {
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
		return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` + projectionTestHead + `","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	got, _, err := projectBranch(cfg, "", Item{Title: "change"}, "forest/7-change", "body", projectionTestHead)
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

func TestProjectBranchRejectsOpenRequestWithoutExpectedHead(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	projectionCommand = func(args ...string) ([]byte, error) {
		return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
	}
	const reviewed = "0123456789abcdef0123456789abcdef01234567"
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if _, _, err := projectBranch(cfg, "", Item{Title: "change"}, "forest/7-change", "body", reviewed); !errors.Is(err, errHostMergeUnavailable) {
		t.Fatalf("open Projection without head = %v, want exact-Revision refusal", err)
	}
}

func TestProjectBranchRejectsIncompleteOpenRequestIdentity(t *testing.T) {
	tests := map[string]string{
		"number": `[{"url":"https://github.com/owner/repo/pull/23","headRefOid":"` + projectionTestHead + `","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`,
		"URL":    `[{"number":23,"headRefOid":"` + projectionTestHead + `","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`,
	}
	for name, response := range tests {
		t.Run(name, func(t *testing.T) {
			old := projectionCommand
			defer func() { projectionCommand = old }()
			projectionCommand = func(args ...string) ([]byte, error) {
				return []byte(response), nil
			}
			cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
			if _, _, err := projectBranch(cfg, "", Item{Title: "change"}, "forest/7-change", "body", projectionTestHead); !errors.Is(err, errHostMergeUnavailable) {
				t.Fatalf("open Projection without %s = %v, want refusal", name, err)
			}
		})
	}
}

func TestProjectionRejectsMissingCrossRepositoryIdentity(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	projectionCommand = func(args ...string) ([]byte, error) {
		return []byte(`[{
			"number":23,
			"url":"https://github.com/owner/repo/pull/23",
			"headRefOid":"` + projectionTestHead + `",
			"headRefName":"forest/7-change",
			"baseRefName":"master"
		}]`), nil
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if _, _, err := projectBranch(cfg, "", Item{Title: "change"}, "forest/7-change", "body", projectionTestHead); !errors.Is(err, errHostMergeUnavailable) {
		t.Fatalf("Projection without cross-repository identity = %v, want refusal", err)
	}
}

func TestProjectionRejectsMalformedHeadRefOID(t *testing.T) {
	tests := map[string]string{
		"empty":   "",
		"short":   strings.Repeat("a", 39),
		"long":    strings.Repeat("a", 41),
		"non-hex": "g" + strings.Repeat("a", 39),
		"zero":    strings.Repeat("0", 40),
	}
	for name, oid := range tests {
		t.Run(name, func(t *testing.T) {
			old := projectionCommand
			defer func() { projectionCommand = old }()
			projectionCommand = func(args ...string) ([]byte, error) {
				return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` +
					oid + `","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
			}
			cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
			if _, err := projectionPRs(cfg, "forest/7-change"); !errors.Is(err, errHostMergeUnavailable) {
				t.Fatalf("malformed HeadRefOID %q = %v, want unavailable", oid, err)
			}
		})
	}
}

func TestMergedProjectionRejectsMalformedTimestamp(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	projectionCommand = func(args ...string) ([]byte, error) {
		return mergedProjectionPage(`{"number":23,"html_url":"https://github.com/owner/repo/pull/23","merged_at":"not-a-timestamp","head":{"sha":"` +
			projectionTestHead + `","ref":"forest/7-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if _, err := mergedProjectionPRs(cfg, "forest/7-change"); !errors.Is(err, errHostMergeUnavailable) {
		t.Fatalf("malformed merged timestamp = %v, want unavailable", err)
	}
}

func TestInspectProjectMergeRejectsOpenRequestWithoutExpectedHead(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "api" {
			return []byte(`[]`), nil
		}
		return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
	}
	const reviewed = "0123456789abcdef0123456789abcdef01234567"
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if _, _, err := inspectProjectMerge(cfg, "forest/7-change", "squash", reviewed); !errors.Is(err, errHostMergeUnavailable) {
		t.Fatalf("Host merge without reported head = %v, want exact-Revision refusal", err)
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
	url, merged, err := projectBranch(cfg, "", Item{Title: "change"}, "forest/7-change", "body", reviewed)
	if err != nil || !merged || url != "https://github.com/owner/repo/pull/23" {
		t.Fatalf("merged Projection recovery = (%q, %v, %v)", url, merged, err)
	}
	cfg.Projection.MergeViaHost = false
	url, merged, err = projectBranch(cfg, "", Item{Title: "change"}, "forest/7-change", "body", reviewed)
	if err != nil || merged || url != "https://github.com/owner/repo/pull/23" {
		t.Fatalf("one-way Projection recovery = (%q, %v, %v)", url, merged, err)
	}
	if createCalls != 0 {
		t.Fatalf("merged Projection recovery created %d duplicate request(s)", createCalls)
	}
}

func TestProjectBranchCreatesMissingRequest(t *testing.T) {
	const branch = "forest/7-change"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var createArgs []string
	created := false
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list" && !created:
			return []byte(`[]`), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":24,"url":"https://github.com/owner/repo/pull/24","headRefOid":"` +
				reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "api":
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "create":
			createArgs = append([]string(nil), args...)
			created = true
			return []byte("https://github.com/owner/repo/pull/24\n"), nil
		default:
			return nil, errors.New("unexpected host command")
		}
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	const secret = "AKIA1234567890ABCDEF"
	got, _, err := projectBranch(cfg, repo, Item{Title: "change " + secret},
		branch, "body "+secret, reviewed)
	if err != nil || got != "https://github.com/owner/repo/pull/24" {
		t.Fatalf("projectBranch create = (%q, %v)", got, err)
	}
	if !hasArgumentPair(createArgs, "--base", "master") ||
		!hasArgumentPair(createArgs, "--head", branch) {
		t.Fatalf("projection create args = %v, want master target and exact source", createArgs)
	}
	if !hasArgumentPair(createArgs, "--title", "forest: change "+secretRedacted) ||
		!hasArgumentPair(createArgs, "--body", "body "+secretRedacted) {
		t.Fatalf("projection create args retained mutable secret-shaped text: %v", createArgs)
	}
}

func TestProjectBranchRejectsMoveBeforeCreate(t *testing.T) {
	const branch = "forest/36-create-race"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	old := projectionCommand
	defer func() { projectionCommand = old }()
	createCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[]`), nil
		case args[0] == "api":
			rebaseTestWriteFile(t, repo+"/moved.txt", "moved\n")
			runGitTest(t, repo, "add", "moved.txt")
			runGitTest(t, repo, "commit", "-q", "-m", "move before create")
			runGitTest(t, repo, "push", "-q", "origin", branch)
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "create":
			createCalls++
			return nil, nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if _, _, err := projectBranch(cfg, repo, Item{Title: "change"}, branch, "body", reviewed); !errors.Is(err, errHostMergeUnavailable) {
		t.Fatalf("Projection create after branch move = %v, want exact-Revision refusal", err)
	}
	if createCalls != 0 {
		t.Fatalf("branch move issued %d stale Projection creates", createCalls)
	}
}

func TestProjectBranchRejectsMoveDuringCreate(t *testing.T) {
	const branch = "forest/37-create-interleave"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	old := projectionCommand
	defer func() { projectionCommand = old }()
	created := false
	advanced := ""
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list" && !created:
			return []byte(`[]`), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":37,"url":"https://github.com/owner/repo/pull/37","headRefOid":"` +
				advanced + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "api":
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "create":
			rebaseTestWriteFile(t, repo+"/moved-during-create.txt", "moved\n")
			runGitTest(t, repo, "add", "moved-during-create.txt")
			runGitTest(t, repo, "commit", "-q", "-m", "move during create")
			runGitTest(t, repo, "push", "-q", "origin", branch)
			advanced = remoteBranchHead(t, repo, branch)
			created = true
			return []byte("https://github.com/owner/repo/pull/37"), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if _, _, err := projectBranch(cfg, repo, Item{Title: "change"}, branch, "body", reviewed); !errors.Is(err, errHostMergeUnavailable) {
		t.Fatalf("Projection create interleave = %v, want post-create Revision refusal", err)
	}
	if !created || advanced == reviewed {
		t.Fatal("create interleave did not advance the branch")
	}
}

func TestProjectBranchHardBrakesMalformedPostCreateReconciliation(t *testing.T) {
	const branch = "forest/38-malformed-reconciliation"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	old := projectionCommand
	defer func() { projectionCommand = old }()
	created := false
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list" && !created:
			return []byte(`[]`), nil
		case args[0] == "api":
			return []byte(`[[]]`), nil
		case args[0] == "pr" && args[1] == "create":
			created = true
			return []byte("https://github.com/owner/repo/pull/38"), nil
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":38,"url":"https://github.com/owner/repo/pull/38","headRefOid":"not-an-oid","headRefName":"` +
				branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
		default:
			return nil, errors.New("unexpected host command")
		}
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if _, _, err := projectBranch(cfg, repo, Item{Title: "change"}, branch, "body", reviewed); !errors.Is(err, errHostMergeUnavailable) || errors.Is(err, errHostMergePending) {
		t.Fatalf("malformed post-create reconciliation = %v, want unavailable hard brake", err)
	}
	if !created {
		t.Fatal("Projection create was not attempted")
	}
}

func TestProjectVerdictCommentContainsDecisionAndChecks(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var commentArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "list" {
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` + projectionTestHead + `","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		if args[0] == "api" && hasArgumentPair(args, "--method", "GET") {
			return []byte(`[[]]`), nil
		}
		if args[0] == "api" && hasArgumentPair(args, "--method", "POST") {
			commentArgs = append([]string(nil), args...)
			return nil, nil
		}
		return nil, errors.New("unexpected host command")
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	err := projectVerdict(cfg, "forest/7-change", projectionTestHead, verdictNote{Verdict: "changes", Notes: "repair the parser"}, checksNote{
		Status:  "fail",
		Results: []checkResult{{Name: "test", Code: 1, Seconds: 2.5, Output: "assertion failed"}},
	})
	if err != nil {
		t.Fatalf("projectVerdict: %v", err)
	}
	if !hasArgumentPair(commentArgs, "--field", "event=COMMENT") ||
		!hasArgumentPair(commentArgs, "--field", "commit_id="+projectionTestHead) {
		t.Errorf("comment args do not bind COMMENT review to Revision %s: %v", projectionTestHead, commentArgs)
	}
	body := strings.Join(commentArgs, "\n")
	for _, want := range []string{
		projectionTestHead, "changes", "repair the parser", "fail", "test", "assertion failed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("comment body missing %q: %s", want, body)
		}
	}
}

func TestProjectCommentReconcilesAcceptedResponseLoss(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	accepted := false
	postCalls := 0
	publishedBody := ""
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` +
				projectionTestHead +
				`","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "GET"):
			if !accepted {
				return []byte(`[[]]`), nil
			}
			return json.Marshal([][]projectionReview{{{
				Body: publishedBody, CommitID: projectionTestHead,
			}}})
		case args[0] == "api" && hasArgumentPair(args, "--method", "POST"):
			postCalls++
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--field" && strings.HasPrefix(args[i+1], "body=") {
					publishedBody = strings.TrimPrefix(args[i+1], "body=")
				}
			}
			accepted = true
			return nil, errors.New("response lost after acceptance")
		default:
			return nil, errors.New("unexpected host command")
		}
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	for range 2 {
		if err := projectVerdict(cfg, "forest/7-change", projectionTestHead,
			verdictNote{Verdict: "changes", Notes: "repair"},
			checksNote{Status: "pass"}); err != nil {
			t.Fatalf("reconciled Projection comment: %v", err)
		}
	}
	if postCalls != 1 {
		t.Fatalf("Projection comment posts = %d, want one", postCalls)
	}
}

func TestProjectCommentHardBrakesMalformedReconciliation(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	reads := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		switch {
		case args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` +
				projectionTestHead +
				`","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "GET"):
			reads++
			if reads == 1 {
				return []byte(`[[]]`), nil
			}
			return []byte(`malformed`), nil
		case args[0] == "api" && hasArgumentPair(args, "--method", "POST"):
			return nil, errors.New("response lost")
		default:
			return nil, errors.New("unexpected host command")
		}
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	err := projectVerdict(cfg, "forest/7-change", projectionTestHead,
		verdictNote{Verdict: "approve"}, checksNote{Status: "pass"})
	if !errors.Is(err, errHostMergeUnavailable) || errors.Is(err, errHostMergePending) {
		t.Fatalf("malformed reconciliation error = %v, want unavailable hard brake", err)
	}
}

func TestProjectionRejectsNullOrNonArrayResponses(t *testing.T) {
	cases := []struct {
		name string
		call func(Config) error
	}{
		{name: "open", call: func(cfg Config) error {
			_, err := projectionPRs(cfg, "forest/7-change")
			return err
		}},
		{name: "merged", call: func(cfg Config) error {
			_, err := mergedProjectionPRs(cfg, "forest/7-change")
			return err
		}},
		{name: "comments", call: func(cfg Config) error {
			_, err := projectionCommentExists(cfg, 23, projectionTestHead, "body")
			return err
		}},
	}
	responses := map[string]string{
		"null":        "null",
		"object":      `{"items":[]}`,
		"null-page":   `[null]`,
		"null-entry":  `[[null]]`,
		"empty-entry": `[[{}]]`,
	}
	for responseName, response := range responses {
		for _, tc := range cases {
			t.Run(responseName+"/"+tc.name, func(t *testing.T) {
				old := projectionCommand
				defer func() { projectionCommand = old }()
				projectionCommand = func(args ...string) ([]byte, error) {
					return []byte(response), nil
				}
				cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
				if err := tc.call(cfg); !errors.Is(err, errHostMergeUnavailable) {
					t.Fatalf("%s %s response = %v, want unavailable", tc.name, responseName, err)
				}
			})
		}
	}
}

func TestProjectChecksRedactsSecretShapedCheckName(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	var commentArgs []string
	projectionCommand = func(args ...string) ([]byte, error) {
		if args[0] == "pr" && args[1] == "list" {
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` + projectionTestHead + `","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
		}
		if args[0] == "api" && hasArgumentPair(args, "--method", "GET") {
			return []byte(`[[]]`), nil
		}
		if args[0] == "api" && hasArgumentPair(args, "--method", "POST") {
			commentArgs = append([]string(nil), args...)
			return nil, nil
		}
		return nil, errors.New("unexpected host command")
	}

	const secret = "sk-AAAAAAAAAAAAAAAA"
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	err := projectChecks(cfg, "forest/7-change", projectionTestHead, checksNote{
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
		if args[0] == "api" {
			comments++
		}
		return []byte(`[]`), nil
	}

	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true}}
	if err := projectVerdict(cfg, "forest/7-change", projectionTestHead, verdictNote{Verdict: "approve"}, checksNote{Status: "pass"}); err != nil {
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
			open:   `[{"number":23,"url":"https://github.com/fork/repo/pull/23","headRefOid":"0123456789abcdef0123456789abcdef01234567","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":true}]`,
			merged: `[[{"number":23,"html_url":"https://github.com/fork/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"0123456789abcdef0123456789abcdef01234567","ref":"forest/7-change","repo":{"full_name":"fork/repo"}},"base":{"ref":"master"}}]]`,
		},
		"wrong branch": {
			open:   `[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"0123456789abcdef0123456789abcdef01234567","headRefName":"forest/8-change","baseRefName":"master","isCrossRepository":false}]`,
			merged: `[[{"number":23,"html_url":"https://github.com/owner/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"0123456789abcdef0123456789abcdef01234567","ref":"forest/8-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}]]`,
		},
		"wrong target": {
			open:   `[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"0123456789abcdef0123456789abcdef01234567","headRefName":"forest/7-change","baseRefName":"release","isCrossRepository":false}]`,
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
			{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"0123456789abcdef0123456789abcdef01234567","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false},
			{"number":24,"url":"https://github.com/owner/repo/pull/24","headRefOid":"0123456789abcdef0123456789abcdef01234567","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}
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

	err := projectMerge(Config{Repo: "owner/repo"}, "forest/7-change", "squash", projectionTestHead)
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
		case len(args) > 0 && args[0] == "api" && hostMerged:
			return mergedProjectionPage(`{"number":23,"html_url":"https://github.com/owner/repo/pull/23","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"forest/7-change","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			return []byte(`[{"number":23,"url":"https://github.com/owner/repo/pull/23","headRefOid":"` + reviewed + `","headRefName":"forest/7-change","baseRefName":"master","isCrossRepository":false}]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
			mergeCalls++
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

func TestProjectMergeNoViewReturnsPendingWithoutWrites(t *testing.T) {
	old := projectionCommand
	defer func() { projectionCommand = old }()
	const reviewed = "0123456789abcdef0123456789abcdef01234567"
	createCalls, mergeCalls := 0, 0
	projectionCommand = func(args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "api" {
			return []byte(`[]`), nil
		}
		if len(args) >= 2 && args[0] == "pr" && args[1] == "list" {
			return []byte(`[]`), nil
		}
		if len(args) >= 2 && args[0] == "pr" && args[1] == "create" {
			createCalls++
		}
		if len(args) >= 2 && args[0] == "pr" && args[1] == "merge" {
			mergeCalls++
		}
		return nil, errors.New("unexpected Host write")
	}
	cfg := Config{Repo: "owner/repo", Projection: ProjectionConfig{Enabled: true, MergeViaHost: true}}
	if err := projectMerge(cfg, "forest/7-change", "squash", reviewed); !errors.Is(err, errHostMergePending) {
		t.Fatalf("no-view projectMerge = %v, want pending", err)
	}
	if createCalls != 0 || mergeCalls != 0 {
		t.Fatalf("no-view projectMerge writes = create %d, merge %d; want zero", createCalls, mergeCalls)
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
