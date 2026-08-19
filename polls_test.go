package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestBuilderPollMatrix(t *testing.T) {
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name   string
		issues string
		branch string
		ghErr  error
		gitErr error
		want   int
	}{
		{name: "empty tracker", issues: `[]`, want: 1},
		{name: "many unready", issues: `[[{"number":99,"pull_request":{}},{"number":100,"pull_request":{}}],[{"number":101,"pull_request":{}}]]`, want: 1},
		{name: "ready", issues: `[[{"number":4}]]`, want: 0},
		{name: "ready branch active", issues: `[[{"number":4}]]`, branch: sha + " refs/heads/forest/4/work\n", want: 1},
		{name: "malformed issue output", issues: `not json`, want: 2},
		{name: "malformed branch output", issues: `[[{"number":4}]]`, branch: "not a branch\n", want: 2},
		{name: "nested issue branch", issues: `[[{"number":4}]]`, branch: sha + " refs/heads/forest/4/work/nested\n", want: 2},
		{name: "forge outage", ghErr: errors.New("offline"), want: 2},
		{name: "git outage", issues: `[[{"number":4}]]`, gitErr: errors.New("git offline"), want: 2},
		{name: "timeout", ghErr: context.DeadlineExceeded, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("POWDER_AGENT", "")
			p := &Poller{Root: t.TempDir(), Repo: "owner/name"}
			p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "gh" {
					if !slices.Contains(args, "--paginate") || !slices.Contains(args, "--slurp") {
						t.Fatalf("pagination args missing: %v", args)
					}
					if args[len(args)-1] != "repos/owner/name/issues?state=open&labels=forest%3Aready&per_page=100" {
						t.Fatalf("repository or label args missing: %v", args)
					}
					return []byte(tc.issues), tc.ghErr
				}
				return []byte(tc.branch), tc.gitErr
			}
			if got, _ := p.builder(ctx); got != tc.want {
				t.Fatalf("poll exit=%d want %d", got, tc.want)
			}
		})
	}
}

func TestBuilderPollIgnoresPowderWithoutAgent(t *testing.T) {
	t.Setenv("POWDER_AGENT", "")
	t.Setenv("POWDER_URL", "https://powder.example")
	p := &Poller{Root: t.TempDir(), Repo: "owner/name"}
	p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "powder" {
			t.Fatalf("powder invoked without POWDER_AGENT: %v", args)
		}
		if name == "gh" {
			return []byte(`[]`), nil
		}
		return nil, nil
	}
	if got, err := p.builder(context.Background()); got != 1 || err != nil {
		t.Fatalf("poll exit=%d err=%v want 1", got, err)
	}
}

func TestBuilderPollPowderRequiresOrigin(t *testing.T) {
	t.Setenv("POWDER_AGENT", "forest-owner-name")
	t.Setenv("POWDER_URL", "")
	t.Setenv("POWDER_API_BASE_URL", "")
	p := &Poller{Root: t.TempDir(), Repo: "owner/name"}
	p.Run = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "gh" {
			return []byte(`[]`), nil
		}
		t.Fatalf("unexpected tool %s", name)
		return nil, nil
	}
	got, err := p.builder(context.Background())
	if got != 2 || err == nil {
		t.Fatalf("poll exit=%d err=%v want 2", got, err)
	}
}

func TestBuilderPollPowderMatrix(t *testing.T) {
	t.Setenv("POWDER_AGENT", "forest-owner-name")
	t.Setenv("POWDER_URL", "https://powder.example")
	ctx := context.Background()
	sha := strings.Repeat("a", 40)
	cases := []struct {
		name      string
		issues    string
		takeable  string
		mine      string
		branch    string
		powderErr error
		want      int
	}{
		{name: "takeable", issues: `[]`, takeable: `[{"id":"iron-forest-ready"}]`, mine: `[]`, want: 0},
		{name: "takeable claimed", issues: `[]`, takeable: `[{"id":"iron-forest-ready"}]`, mine: `[]`, branch: sha + " refs/heads/forest/iron-forest-ready/work\n", want: 1},
		{name: "mine unclaimed", issues: `[]`, takeable: `[]`, mine: `[{"id":"iron-forest-held"}]`, want: 0},
		{name: "github ready empty powder", issues: `[[{"number":4}]]`, takeable: `[]`, mine: `[]`, want: 0},
		{name: "malformed powder", issues: `[]`, takeable: `not json`, mine: `[]`, want: 2},
		{name: "bad powder id", issues: `[]`, takeable: `[{"id":"bad id"}]`, mine: `[]`, want: 2},
		{name: "powder outage", issues: `[]`, takeable: `[]`, mine: `[]`, powderErr: errors.New("offline"), want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &Poller{Root: t.TempDir(), Repo: "owner/name"}
			p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "gh" {
					return []byte(tc.issues), nil
				}
				if name == "powder" {
					if !slices.Contains(args, "list") || !slices.Contains(args, "--repo") || !slices.Contains(args, "owner/name") {
						t.Fatalf("powder list repo args missing: %v", args)
					}
					if slices.Contains(args, "--takeable") {
						return []byte(tc.takeable), tc.powderErr
					}
					if slices.Contains(args, "--mine") {
						if !slices.Contains(args, "forest-owner-name") {
							t.Fatalf("powder mine agent missing: %v", args)
						}
						return []byte(tc.mine), tc.powderErr
					}
					t.Fatalf("unexpected powder args: %v", args)
				}
				return []byte(tc.branch), nil
			}
			if got, _ := p.builder(ctx); got != tc.want {
				t.Fatalf("poll exit=%d want %d", got, tc.want)
			}
		})
	}
}

func TestParseBranchOutputSkipsLeftoverHyphenTips(t *testing.T) {
	sha := strings.Repeat("a", 40)
	output := []byte(sha + " refs/heads/forest/259-cancel-live-run-cli\n" + sha + " refs/heads/forest/4/work\n")
	got, err := parseBranchOutput(output, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "forest/4/work" {
		t.Fatalf("tips=%#v", got)
	}
}

func TestVerifierPollSkipsLeftoverHyphenTips(t *testing.T) {
	sha := strings.Repeat("a", 40)
	p := &Poller{Root: t.TempDir(), Repo: "owner/name"}
	p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "git" && slices.Contains(args, "refs/heads/forest/*") {
			return []byte(sha + " refs/heads/forest/259-cancel-live-run-cli\n" + sha + " refs/heads/forest/284-evidence-ref-selection\n"), nil
		}
		t.Fatalf("unexpected %s %v", name, args)
		return nil, nil
	}
	code, err := p.verifier(context.Background())
	if code != 1 || err != nil {
		t.Fatalf("poll exit=%d err=%v want 1", code, err)
	}
}

func TestExactGitLine(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "LF preserves actor bytes", input: " Iron Forest Builder\x00builder@forest.invalid \n", want: " Iron Forest Builder\x00builder@forest.invalid "},
		{name: "CRLF", input: "Iron Forest Builder\x00builder@forest.invalid\r\n", want: "Iron Forest Builder\x00builder@forest.invalid"},
		{name: "missing terminator", input: "Iron Forest Builder\x00builder@forest.invalid", wantErr: true},
		{name: "second record", input: "Iron Forest Builder\x00builder@forest.invalid\nother\n", wantErr: true},
		{name: "embedded CR", input: "Iron Forest\rBuilder\x00builder@forest.invalid\n", wantErr: true},
		{name: "extra CR", input: "Iron Forest Builder\x00builder@forest.invalid\r\r\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := exactGitLine([]byte(tc.input))
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("exactGitLine=%q err=%v, want %q err=%v", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func pollChecksNote(sha, result string) string {
	return `{"schema":"forest.checks.v1","revision":"` + sha + `","results":[` + result + `],"time":"2026-08-10T00:00:00Z"}`
}

func pollVerdictNote(sha, verdict string) string {
	return `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"` + verdict + `","summary":"done","time":"2026-08-10T00:00:00Z"}`
}

func TestPollNoteDecodersRejectStrictJSON(t *testing.T) {
	sha := strings.Repeat("e", 40)
	validReview := pollReviewNote(sha)
	validChecks := pollChecksNote(sha, `{"name":"test","ok":true,"exit":0}`)
	validVerdict := pollVerdictNote(sha, "approve")
	decodeReviewNote := func(data []byte) error { _, err := decodeReview(data, sha); return err }
	decodeChecksNote := func(data []byte) error { _, err := decodeChecks(data, sha); return err }
	decodeVerdictNote := func(data []byte) error { _, err := decodeVerdict(data, sha); return err }
	if err := decodeReviewNote([]byte(validReview)); err != nil {
		t.Fatalf("valid review: %v", err)
	}
	if err := decodeChecksNote([]byte(validChecks)); err != nil {
		t.Fatalf("valid checks: %v", err)
	}

	objects := []struct {
		name   string
		data   string
		keys   []string
		decode func([]byte) error
	}{
		{name: "review", data: validReview, keys: []string{"schema", "subject", "branch", "revision", "time"}, decode: decodeReviewNote},
		{name: "checks", data: validChecks, keys: []string{"schema", "revision", "results", "time", "name", "ok", "exit"}, decode: decodeChecksNote},
		{name: "verdict", data: validVerdict, keys: []string{"schema", "revision", "verdict", "summary", "time"}, decode: decodeVerdictNote},
	}
	for _, object := range objects {
		for _, key := range object.keys {
			alias := strings.ToUpper(key[:1]) + key[1:]
			data := strings.Replace(object.data, `"`+key+`":`, `"`+alias+`":`, 1)
			t.Run(object.name+" "+key+" alias", func(t *testing.T) {
				if err := object.decode([]byte(data)); err == nil {
					t.Fatalf("accepted case-folded alias %q", alias)
				}
			})
		}
	}

	missingReview := `{"schema":"forest.review-request.v2","subject":"4","branch":"forest/4/work","revision":"` + sha + `"}`
	missingChecks := `{"schema":"forest.checks.v1","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`
	missingVerdict := `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"approve","time":"2026-08-10T00:00:00Z"}`
	nullResults := strings.Replace(validChecks, `"results":[{"name":"test","ok":true,"exit":0}]`, `"results":null`, 1)
	cases := []struct {
		name   string
		data   string
		decode func([]byte) error
	}{
		{name: "review unknown member", data: strings.Replace(validReview, `,"time":`, `,"extra":true,"time":`, 1), decode: decodeReviewNote},
		{name: "checks unknown nested member", data: pollChecksNote(sha, `{"name":"test","ok":true,"exit":0,"extra":true}`), decode: decodeChecksNote},
		{name: "verdict unknown member", data: strings.Replace(validVerdict, `,"time":`, `,"extra":true,"time":`, 1), decode: decodeVerdictNote},
		{name: "review duplicate member", data: `{"schema":"forest.review-request.v2","schema":"forest.review-request.v2","subject":"4","branch":"forest/4/work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`, decode: decodeReviewNote},
		{name: "checks duplicate nested member", data: pollChecksNote(sha, `{"name":"test","name":"test","ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "verdict duplicate member", data: `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"approve","summary":"done","summary":"done","time":"2026-08-10T00:00:00Z"}`, decode: decodeVerdictNote},
		{name: "review mixed-case duplicate", data: strings.Replace(validReview, `"schema":"forest.review-request.v2"`, `"schema":"bad","Schema":"forest.review-request.v2"`, 1), decode: decodeReviewNote},
		{name: "check mixed-case duplicate", data: pollChecksNote(sha, `{"name":"bad","Name":"test","ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "trailing JSON", data: validReview + ` {}`, decode: decodeReviewNote},
		{name: "review malformed time", data: strings.Replace(validReview, "2026-08-10T00:00:00Z", "not-a-time", 1), decode: decodeReviewNote},
		{name: "checks malformed time", data: strings.Replace(validChecks, "2026-08-10T00:00:00Z", "not-a-time", 1), decode: decodeChecksNote},
		{name: "verdict malformed time", data: strings.Replace(validVerdict, "2026-08-10T00:00:00Z", "not-a-time", 1), decode: decodeVerdictNote},
		{name: "verdict blank summary", data: strings.Replace(validVerdict, `"summary":"done"`, `"summary":"  "`, 1), decode: decodeVerdictNote},
		{name: "review empty branch slug", data: pollReviewNoteBranch(sha, "forest/4/"), decode: decodeReviewNote},
		{name: "review uppercase branch slug", data: pollReviewNoteBranch(sha, "forest/4/Bad"), decode: decodeReviewNote},
		{name: "review underscored branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad_name"), decode: decodeReviewNote},
		{name: "review leading branch separator", data: pollReviewNoteBranch(sha, "forest/4/-bad"), decode: decodeReviewNote},
		{name: "review repeated branch separator", data: pollReviewNoteBranch(sha, "forest/4/bad--name"), decode: decodeReviewNote},
		{name: "review trailing branch separator", data: pollReviewNoteBranch(sha, "forest/4/bad-"), decode: decodeReviewNote},
		{name: "review spaced branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad name"), decode: decodeReviewNote},
		{name: "review dotted branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad.name"), decode: decodeReviewNote},
		{name: "review leading dot branch", data: pollReviewNoteBranch(sha, "forest/4/.bad"), decode: decodeReviewNote},
		{name: "review dot sequence branch", data: pollReviewNoteBranch(sha, "forest/4/bad..name"), decode: decodeReviewNote},
		{name: "review nested branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad/name"), decode: decodeReviewNote},
		{name: "review tilde branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad~name"), decode: decodeReviewNote},
		{name: "review caret branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad^name"), decode: decodeReviewNote},
		{name: "review colon branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad:name"), decode: decodeReviewNote},
		{name: "review question branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad?name"), decode: decodeReviewNote},
		{name: "review asterisk branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad*name"), decode: decodeReviewNote},
		{name: "review bracket branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad[name"), decode: decodeReviewNote},
		{name: "review single-at branch slug", data: pollReviewNoteBranch(sha, "forest/4/@"), decode: decodeReviewNote},
		{name: "review backslash branch slug", data: strings.Replace(validReview, "forest/4/work", `forest/4/bad\\name`, 1), decode: decodeReviewNote},
		{name: "review control branch slug", data: strings.Replace(validReview, "forest/4/work", "forest/4/bad\u0001name", 1), decode: decodeReviewNote},
		{name: "review at-brace branch slug", data: pollReviewNoteBranch(sha, "forest/4/bad@{name"), decode: decodeReviewNote},
		{name: "review trailing dot branch", data: pollReviewNoteBranch(sha, "forest/4/bad."), decode: decodeReviewNote},
		{name: "review lock suffix branch", data: pollReviewNoteBranch(sha, "forest/4/bad.lock"), decode: decodeReviewNote},
		{name: "missing review member", data: missingReview, decode: decodeReviewNote},
		{name: "missing checks member", data: missingChecks, decode: decodeChecksNote},
		{name: "missing verdict member", data: missingVerdict, decode: decodeVerdictNote},
		{name: "null checks results", data: nullResults, decode: decodeChecksNote},
		{name: "empty checks results", data: pollChecksNote(sha, ""), decode: decodeChecksNote},
		{name: "null check result member", data: pollChecksNote(sha, `{"name":"test","ok":null,"exit":0}`), decode: decodeChecksNote},
		{name: "empty check result member", data: pollChecksNote(sha, `{"name":"","ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "check result missing name", data: pollChecksNote(sha, `{"ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "check result missing ok", data: pollChecksNote(sha, `{"name":"test","exit":0}`), decode: decodeChecksNote},
		{name: "check result missing exit", data: pollChecksNote(sha, `{"name":"test","ok":true}`), decode: decodeChecksNote},
		{name: "negative check result exit", data: pollChecksNote(sha, `{"name":"test","ok":false,"exit":-1}`), decode: decodeChecksNote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.decode([]byte(tc.data)); err == nil {
				t.Fatalf("accepted malformed %s", tc.name)
			}
		})
	}
}

func TestPollRealToolStopsDescendants(t *testing.T) {
	testGitTransportStopsDescendants(t, "Poll", "poll-output", func(ctx context.Context, root string) ([]byte, error) {
		return NewPoller(root, "owner/name").git(ctx, "--version")
	})
}
