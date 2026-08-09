package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMergeViaHostRecoversTrackerCloseFailure(t *testing.T) {
	branch := "forest/9-host-recovery"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)

	oldProjection := projectionCommand
	defer func() { projectionCommand = oldProjection }()
	merged := false
	mergeCalls := 0
	projectionCommand = func(args ...string) ([]byte, error) {
		state := ""
		for i := 0; i+1 < len(args); i++ {
			if args[i] == "--state" {
				state = args[i+1]
			}
		}
		switch {
		case len(args) > 0 && args[0] == "api" && merged:
			return mergedProjectionPage(`{"number":9,"html_url":"https://github.com/owner/repo/pull/9","merged_at":"2026-08-08T00:00:00Z","head":{"sha":"` + reviewed + `","ref":"` + branch + `","repo":{"full_name":"owner/repo"}},"base":{"ref":"master"}}`), nil
		case len(args) > 0 && args[0] == "api":
			return []byte(`[]`), nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
			merged = true
			mergeCalls++
			if err := deleteRef(repo, "refs/heads/"+branch, reviewed); err != nil {
				return nil, err
			}
			return nil, nil
		case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
			switch {
			case state == "open" && !merged:
				return []byte(`[{"number":9,"url":"https://github.com/owner/repo/pull/9","headRefOid":"` + reviewed + `","headRefName":"` + branch + `","baseRefName":"master","isCrossRepository":false}]`), nil
			case state == "merged" && merged:
				return []byte(`[]`), nil
			default:
				return []byte(`[]`), nil
			}
		default:
			return nil, errors.New("unexpected host command")
		}
	}

	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "list" {
			return []byte(`[]`), nil
		}
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			closeCalls++
			if closeCalls == 1 {
				return nil, errors.New("tracker unavailable")
			}
		}
		return []byte(`{"state":"OPEN"}`), nil
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{Enabled: true, MergeViaHost: true}
	cfg.Flows.Verifier.Merge = "squash"
	cfg.Flows.Verifier.AutoMerge = true
	item := Item{ID: "9", Title: "change"}
	reviewAgent := testVerifierAgent()
	writeApprovalNotes(t, repo, reviewed, reviewAgent)
	if err := mergeVerified(cfg, repo, branch, reviewed, item,
		reviewAgent); err == nil ||
		!strings.Contains(err.Error(), "close item") {
		t.Fatalf("first host merge = %v, want Tracker close failure", err)
	}
	// A landed retirement fact is sufficient after restart. Remove the Host
	// oracle before cloning so recovery cannot reuse process-local merge state
	// or ask the Host to merge a second time.
	projectionCommand = func(...string) ([]byte, error) {
		return nil, errors.New("Host called during landed retirement recovery")
	}
	if out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); out != "" {
		t.Fatalf("host did not auto-delete source branch: %s", out)
	}
	restartRoot := t.TempDir()
	restarted := filepath.Join(restartRoot, "restart")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	runGitTest(t, restartRoot, "clone", "-q", origin, restarted)
	subjects, err := (verifierFlow{}).Select(cfg, restarted)
	if err != nil {
		t.Fatalf("restart select: %v", err)
	}
	if len(subjects) != 1 || subjects[0].Kind != "retirement" {
		t.Fatalf("recovery subjects = %#v, want one retirement", subjects)
	}
	out, err := (verifierFlow{}).Act(cfg, restarted, subjects[0], "recovery-run")
	if err != nil {
		t.Fatalf("host merge recovery: %v", err)
	}
	if out.Status != "merged" {
		t.Fatalf("recovery status = %q, want merged", out.Status)
	}
	agent := testVerifierAgent()
	if out.Agent != agent.Name || out.Model != agent.Model || out.DefSHA != agent.DefSHA {
		t.Fatalf("host recovery attribution = %#v, want marker identity %#v", out, agent)
	}
	if mergeCalls != 1 || closeCalls != 2 {
		t.Fatalf("recovery effects: merge=%d close=%d, want merge=1 close=2", mergeCalls, closeCalls)
	}
	if facts, err := listRetirements(restarted); err != nil || len(facts) != 0 {
		t.Fatalf("retirement facts after recovery = (%#v, %v), want none", facts, err)
	}
	for _, subject := range []string{branch, item.ID} {
		if refs, err := listEffectRefs(restarted, subject); err != nil || len(refs) != 0 {
			t.Fatalf("retired Effect refs for %q = (%v, %v), want none", subject, refs, err)
		}
	}
}
func TestRetirementFactBlocksAdvancedBranch(t *testing.T) {
	branch := "forest/12-advanced"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	if _, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: "12", Transport: "host",
		Strategy: "squash", Title: "advanced", State: "landed",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	}); err != nil {
		t.Fatal(err)
	}
	rebaseTestWriteFile(t, filepath.Join(repo, "later.txt"), "later\n")
	runGitTest(t, repo, "add", "later.txt")
	runGitTest(t, repo, "commit", "-q", "-m", "later")
	runGitTest(t, repo, "push", "-q", "origin", branch)
	advanced := remoteBranchHead(t, repo, branch)

	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	closeCalls := 0
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			closeCalls++
		}
		return []byte(`{}`), nil
	}
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 || subjects[0].Kind != subjectRetirement || subjects[0].Revision != reviewed {
		t.Fatalf("advanced retirement subjects = %#v, want old durable fact", subjects)
	}
	if _, err := (verifierFlow{}).Act(cfg, repo, subjects[0], "advanced-retirement"); err == nil ||
		!strings.Contains(err.Error(), "advanced") {
		t.Fatalf("advanced retirement = %v, want named refusal", err)
	}
	if closeCalls != 0 {
		t.Fatalf("advanced retirement closed Tracker %d time(s)", closeCalls)
	}
	if got := remoteBranchHead(t, repo, branch); got != advanced {
		t.Fatalf("advanced branch = %s, want %s intact", got, advanced)
	}
	for range stalledRunLimit {
		if err := recordStalled(repo, "verifier", retirementSubjectKey(branch), reviewed); err != nil {
			t.Fatal(err)
		}
	}
	subjects, err = (verifierFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 0 {
		t.Fatalf("stalled retirement remained selectable: %#v", subjects)
	}
}

// TestVerifierRetirementTrackerCloseRetriesExactItem drives Verifier Act through
// final retirement cleanup and proves Tracker Close's recovery view stays bound
// to the same Item and repository as the close request.
func TestVerifierRetirementTrackerCloseRetriesExactItem(t *testing.T) {
	branch := "forest/52-tracker-close"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	item := Item{ID: "52", Title: "tracker close"}
	_, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: item.ID, Transport: "git",
		Strategy: "squash", Title: item.Title, State: "landed",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}

	oldTracker := trackerFor
	trackerFor = func(repo string) Tracker { return githubTracker{repo: repo} }
	defer func() { trackerFor = oldTracker }()
	oldGH := ghJSON
	defer func() { ghJSON = oldGH }()
	var calls [][]string
	ghJSON = func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			return nil, errors.New("Tracker close response lost")
		}
		if len(args) >= 2 && args[0] == "issue" && args[1] == "view" {
			return []byte(`{"state":"CLOSED"}`), nil
		}
		return nil, errors.New("unexpected Tracker command")
	}

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed,
		ID: item.ID, Branch: branch, Item: item}
	out, err := (verifierFlow{}).Act(cfg, repo, subject, "tracker-close-retry")
	if err != nil || out.Status != "merged" {
		t.Fatalf("Verifier retirement Act = (%#v, %v), want merged recovery", out, err)
	}
	if len(calls) != 2 || calls[0][0] != "issue" || calls[0][1] != "close" ||
		calls[1][0] != "issue" || calls[1][1] != "view" {
		t.Fatalf("Tracker close recovery calls = %v, want close then view", calls)
	}
	if !hasArgumentPair(calls[0], "-R", cfg.Repo) || !hasArgumentPair(calls[1], "-R", cfg.Repo) ||
		calls[0][len(calls[0])-1] != item.ID || calls[1][2] != item.ID {
		t.Fatalf("Tracker recovery calls = %v, want Item %q in repository %q", calls, item.ID, cfg.Repo)
	}
	if !hasArgumentPair(calls[1], "--json", "state") {
		t.Fatalf("Tracker recovery view = %v, want exact state read", calls[1])
	}
	if _, found, err := readRetirement(repo, branch, reviewed); err != nil || found {
		t.Fatalf("retirement fact after exact Tracker retry = (found=%v, err=%v), want removed", found, err)
	}
}
func TestMalformedTrackerCloseEvidenceRemainsTerminal(t *testing.T) {
	for i, body := range []string{`malformed`, `{}`, `null`} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			branch := fmt.Sprintf("forest/53-malformed-close-%d", i)
			repo, _, reviewed, _ := newVerifierBranch(t, branch)
			agent := testVerifierAgent()
			item := Item{ID: "53", Title: "malformed close"}
			if _, err := recordRetirement(repo, retirementRecord{
				Branch: branch, Revision: reviewed, ItemID: item.ID, Transport: "git",
				Strategy: "squash", Title: item.Title, State: "landed",
				Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
			}); err != nil {
				t.Fatal(err)
			}
			oldTracker := trackerFor
			trackerFor = func(repo string) Tracker { return githubTracker{repo: repo} }
			defer func() { trackerFor = oldTracker }()
			oldGH := ghJSON
			defer func() { ghJSON = oldGH }()
			var calls int
			ghJSON = func(args ...string) ([]byte, error) {
				if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
					calls++
					return nil, errors.New("Tracker close response lost")
				}
				if len(args) >= 2 && args[0] == "issue" && args[1] == "view" {
					calls++
					return []byte(body), nil
				}
				return nil, errors.New("unexpected Tracker command")
			}
			cfg := defaultConfig()
			cfg.Repo = "owner/repo"
			subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement,
				Revision: reviewed, ID: item.ID, Branch: branch, Item: item}
			if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
				t.Fatalf("malformed Tracker close code = %d, want failure", code)
			}
			if stalled, err := stalledOn(repo, (verifierFlow{}).Name(),
				subject.Key, reviewed); err != nil || !stalled {
				t.Fatalf("malformed Tracker close brake = (%v, %v), want terminal", stalled, err)
			}
			if code := actOnSubject(verifierFlow{}, cfg, repo, subject, nil); code != 1 {
				t.Fatalf("braked Tracker close code = %d, want failure", code)
			}
			if calls != 2 {
				t.Fatalf("Tracker close calls = %d, want no retry after malformed evidence", calls)
			}
		})
	}
}

// TestDeleteReviewedBranchReconcilesConcurrentAbsence simulates a Host
// deleting the reviewed branch after inspection but before Forest receives
// the deletion result. The absent branch already satisfies retirement.
func TestDeleteReviewedBranchReconcilesConcurrentAbsence(t *testing.T) {
	branch := "forest/53-concurrent-delete"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	script := `#!/bin/sh
case " $* " in
  *" push "*"refs/heads/$FOREST_DELETE_BRANCH"*)
    "$FOREST_REAL_GIT" -C "$FOREST_DELETE_REPO" push --no-verify origin ":refs/heads/$FOREST_DELETE_BRANCH" >/dev/null 2>&1 || exit $?
    echo "simulated lost delete response" >&2
    exit 1
    ;;
esac
exec "$FOREST_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(wrapperDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FOREST_REAL_GIT", realGit)
	t.Setenv("FOREST_DELETE_REPO", repo)
	t.Setenv("FOREST_DELETE_BRANCH", branch)
	t.Setenv("PATH", wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := deleteReviewedBranch(repo, branch, reviewed); err != nil {
		t.Fatalf("reconciled branch deletion = %v, want success", err)
	}
	if got := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); got != "" {
		t.Fatalf("reconciled branch = %q, want absent", got)
	}
}

// TestVerifierRetirementRetriesFinalRefDeletion drives Verifier Act through a
// failed final retirement-ref delete after every earlier effect succeeded.
func TestVerifierRetirementRetriesFinalRefDeletion(t *testing.T) {
	branch := "forest/53-final-ref"
	repo, _, reviewed, _ := newVerifierBranch(t, branch)
	agent := testVerifierAgent()
	item := Item{ID: "53", Title: "final ref"}
	fact, err := recordRetirement(repo, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: item.ID, Transport: "git",
		Strategy: "squash", Title: item.Title, State: "landed",
		Agent: agent.Name, Model: agent.Model, DefSHA: agent.DefSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bumpAttempts(repo, "branch-"+branch); err != nil {
		t.Fatal(err)
	}
	for _, effect := range []struct{ kind, subject string }{
		{"Projection-comment", branch},
		{"Tracker-builder-comment", item.ID},
	} {
		if err := claimEffect(repo, effect.kind, effect.subject, reviewed); err != nil {
			t.Fatal(err)
		}
	}
	stalls := []struct{ flow, key string }{
		{"builder", "item-" + item.ID},
		{"fixer", "branch-" + branch},
	}
	for _, stall := range stalls {
		if err := recordStalled(repo, stall.flow, stall.key, reviewed); err != nil {
			t.Fatal(err)
		}
	}
	toggle := filepath.Join(t.TempDir(), "allow-final-ref-delete")
	origin := runGitTest(t, repo, "remote", "get-url", "origin")
	hook := filepath.Join(origin, "hooks", "update")
	script := "#!/bin/sh\nif [ \"$1\" = '" + fact.Ref + "' ] && [ ! -e '" + toggle + "' ]; then touch '" + toggle + "'; exit 1; fi\nexit 0\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(hook) })

	tk := newMemoryTracker()
	tk.seed(item)
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	subject := Subject{Key: retirementSubjectKey(branch), Kind: subjectRetirement, Revision: reviewed,
		ID: item.ID, Branch: branch, Item: item}
	out, err := (verifierFlow{}).Act(cfg, repo, subject, "final-ref-first")
	if err == nil || out.Status != "merge_failed" {
		t.Fatalf("first final-ref retirement Act = (%#v, %v), want retryable merge failure", out, err)
	}
	if got := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch); got != "" {
		t.Fatalf("first final-ref retry branch = %q, want deleted", got)
	}
	if _, err := tk.Get(item.ID); err == nil {
		t.Fatal("first final-ref retry left the Tracker Item open")
	}
	if attempts, err := readAttempts(repo, "branch-"+branch); err != nil || attempts != 1 {
		t.Fatalf("first final-ref retry attempts = (%d, %v), want retained atomically", attempts, err)
	}
	if _, found, err := readRetirement(repo, branch, reviewed); err != nil || !found {
		t.Fatalf("first final-ref retry fact = (found=%v, err=%v), want retained recovery evidence", found, err)
	}
	if refs, err := listEffectRefs(repo, item.ID); err != nil || len(refs) != 3 {
		t.Fatalf("atomic failure Item Effect refs = (%v, %v), want three retained", refs, err)
	}
	if refs, err := listEffectRefs(repo, branch); err != nil || len(refs) != 1 {
		t.Fatalf("atomic failure branch Effect refs = (%v, %v), want one retained", refs, err)
	}
	for _, stall := range stalls {
		if sha, _, err := getBlobRef(repo, stalledRef(stall.flow, stall.key)); err != nil || sha == "" {
			t.Fatalf("atomic failure stall %s/%s = (%q, %v), want retained",
				stall.flow, stall.key, sha, err)
		}
	}

	subjects, err := (verifierFlow{}).Select(cfg, repo)
	if err != nil || len(subjects) != 1 || subjects[0].Kind != subjectRetirement {
		t.Fatalf("final-ref retry Select = (%#v, %v), want one retirement Subject", subjects, err)
	}
	out, err = (verifierFlow{}).Act(cfg, repo, subjects[0], "final-ref-retry")
	if err != nil || out.Status != "merged" {
		t.Fatalf("final-ref retry Act = (%#v, %v), want merged cleanup", out, err)
	}
	if _, found, err := readRetirement(repo, branch, reviewed); err != nil || found {
		t.Fatalf("final-ref retry fact = (found=%v, err=%v), want removed", found, err)
	}
	if refs, err := listEffectRefs(repo, item.ID); err != nil || len(refs) != 0 {
		t.Fatalf("final-ref retry Effect refs = (%v, %v), want none", refs, err)
	}
	if refs, err := listEffectRefs(repo, branch); err != nil || len(refs) != 0 {
		t.Fatalf("final-ref retry branch Effect refs = (%v, %v), want none", refs, err)
	}
	for _, stall := range stalls {
		if sha, _, err := getBlobRef(repo, stalledRef(stall.flow, stall.key)); err != nil || sha != "" {
			t.Fatalf("retired stall %s/%s = (%q, %v), want removed",
				stall.flow, stall.key, sha, err)
		}
	}
}
