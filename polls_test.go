package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
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
		{name: "ready branch active", issues: `[[{"number":4}]]`, branch: sha + " refs/heads/forest/4-work\n", want: 1},
		{name: "malformed issue output", issues: `not json`, want: 2},
		{name: "malformed branch output", issues: `[[{"number":4}]]`, branch: "not a branch\n", want: 2},
		{name: "nested issue branch", issues: `[[{"number":4}]]`, branch: sha + " refs/heads/forest/4-work/nested\n", want: 2},
		{name: "forge outage", ghErr: errors.New("offline"), want: 2},
		{name: "git outage", issues: `[[{"number":4}]]`, gitErr: errors.New("git offline"), want: 2},
		{name: "timeout", ghErr: context.DeadlineExceeded, want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			if got := p.builder(ctx); got != tc.want {
				t.Fatalf("poll exit=%d want %d", got, tc.want)
			}
		})
	}
}

func TestConcurrentPollsPreserveCanonicalNotesAcrossLinkedWorktrees(t *testing.T) {
	root, _ := testClone(t)
	master := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "checkout", "-b", "forest/4-work")
	if err := os.WriteFile(filepath.Join(root, "poll-change"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "poll-change")
	runGitDir(t, root, "commit", "-m", "poll change")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/4-work")
	addNote(t, root, reviewRequestNoteRef, tip, pollReviewNote(tip), "Iron Forest Builder", "builder@forest.invalid")
	runGitDir(t, root, "push", "origin", reviewRequestNoteRef+":"+reviewRequestNoteRef)

	addNote(t, root, checksNoteRef, master, pollChecksNote(master, `{"name":"test","ok":true,"exit":0}`), "Iron Forest Verifier", "verifier@forest.invalid")
	addNote(t, root, verdictNoteRef, master, pollVerdictNote(master, "changes"), "Iron Forest Verifier", "verifier@forest.invalid")
	runGitDir(t, root, "push", "origin", checksNoteRef+":"+checksNoteRef, verdictNoteRef+":"+verdictNoteRef)
	before := make(map[string]string)
	for ref := range map[string]struct{}{reviewRequestNoteRef: {}, checksNoteRef: {}, verdictNoteRef: {}} {
		before[ref] = strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
	}

	linked := make([]string, 2)
	for index := range linked {
		linked[index] = filepath.Join(t.TempDir(), "linked")
		runGitDir(t, root, "worktree", "add", "--detach", linked[index], master)
	}

	var fetchMu sync.Mutex
	ready := make(chan struct{}, len(linked))
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	fetched := make([][]string, len(linked))
	cleaned := make([][]string, len(linked))
	results := make(chan int, len(linked))
	for index, worktree := range linked {
		index := index
		poller := NewPoller(worktree, "owner/name")
		run := poller.Run
		poller.Run = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			if slices.Contains(args, "fetch") {
				fetchMu.Lock()
				output, err := run(ctx, name, args...)
				fetchMu.Unlock()
				if err == nil {
					for _, arg := range args {
						refspec := strings.SplitN(arg, ":", 2)
						if len(refspec) == 2 && before[refspec[0]] != "" && strings.HasPrefix(refspec[1], pollNotesNamespace+"/") {
							fetched[index] = append(fetched[index], refspec[1])
						}
					}
				}
				return output, err
			}
			output, err := run(ctx, name, args...)
			if err != nil {
				return output, err
			}
			if slices.Contains(args, "rev-parse") && strings.HasSuffix(args[len(args)-1], "/verdict") {
				ready <- struct{}{}
				<-release
			}
			if slices.Contains(args, "update-ref") && slices.Contains(args, "-d") {
				cleaned[index] = append(cleaned[index], args[len(args)-1])
			}
			return output, nil
		}
		go func() {
			results <- poller.verifier(context.Background())
		}()
	}

	for range linked {
		select {
		case <-ready:
		case got := <-results:
			t.Fatalf("Poll finished with exit %d before both snapshots were ready", got)
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for overlapping Poll snapshots")
		}
	}
	liveOutput := runGitDir(t, root, "for-each-ref", "--format=%(refname)", pollNotesNamespace+"/")
	live := make(map[string]bool)
	for _, ref := range strings.Fields(string(liveOutput)) {
		live[ref] = true
	}
	if len(live) != len(linked)*3 {
		t.Fatalf("live private refs=%v want %d", live, len(linked)*3)
	}
	owner := make(map[string]int, len(live))
	for index, refs := range fetched {
		if len(refs) != 3 {
			t.Fatalf("Poll %d fetched refs=%v want 3", index, refs)
		}
		for _, ref := range refs {
			if other, exists := owner[ref]; exists {
				t.Fatalf("private ref %s shared by Polls %d and %d", ref, other, index)
			}
			if !live[ref] {
				t.Fatalf("Poll %d private ref %s is not live at barrier", index, ref)
			}
			owner[ref] = index
		}
	}

	close(release)
	released = true
	for range linked {
		select {
		case got := <-results:
			if got != 0 {
				t.Fatalf("poll exit=%d want 0", got)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("timed out waiting for concurrent Poll")
		}
	}
	for index := range cleaned {
		if !slices.Equal(cleaned[index], fetched[index]) {
			t.Fatalf("Poll %d cleanup refs=%v want %v", index, cleaned[index], fetched[index])
		}
	}
	for ref, oid := range before {
		after := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", ref)))
		if after != oid {
			t.Fatalf("%s changed from %s to %s", ref, oid, after)
		}
	}
	if refs := strings.TrimSpace(string(runGitDir(t, root, "for-each-ref", "--format=%(refname)", pollNotesNamespace+"/"))); refs != "" {
		t.Fatalf("private Poll refs remain: %s", refs)
	}
}

func TestPollRejectsDuplicateNoteBlobFromWrongTargetWriter(t *testing.T) {
	root, _ := testClone(t)
	master := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "checkout", "-b", "forest/4-work")
	if err := os.WriteFile(filepath.Join(root, "poll-change"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "poll-change")
	runGitDir(t, root, "commit", "-m", "poll change")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/4-work")

	payload := pollReviewNote(tip)
	addNote(t, root, reviewRequestNoteRef, tip, payload, "Operator", "operator@example.invalid")
	addNote(t, root, reviewRequestNoteRef, master, payload, "Iron Forest Builder", "builder@forest.invalid")
	runGitDir(t, root, "push", "origin", reviewRequestNoteRef+":"+reviewRequestNoteRef)

	if got := NewPoller(root, "owner/name").verifier(context.Background()); got != 2 {
		t.Fatalf("poll exit=%d want 2", got)
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

type notePollCase struct {
	name              string
	branches          string
	noteRefs          string
	reAdvertised      string
	reAdvertisedNotes string
	review            string
	verdict           string
	reviewIdentity    string
	verdictIdentity   string
	noteTarget        string
	notePaths         string
	fetchedOID        string
	branchErr         error
	reAdvertiseErr    error
	fetchErr          error
	actualFetchErr    error
	revParseErr       error
	reviewErr         error
	verdictErr        error
	treeErr           error
	logErr            error
	missingReview     bool
	missingVerdict    bool
	deletedRefs       *[]string
	want              int
}

func pollVerdictNote(sha, verdict string) string {
	return `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"` + verdict + `","summary":"done","time":"2026-08-10T00:00:00Z"}`
}

func notePoller(t *testing.T, tc notePollCase) *Poller {
	t.Helper()
	privateCanonical := make(map[string]string)
	completedReads := make(map[string]bool)
	readTargets := make(map[string]string)
	allNoteRefs := func(args []string) bool {
		return slices.Contains(args, reviewRequestNoteRef) &&
			slices.Contains(args, checksNoteRef) &&
			slices.Contains(args, verdictNoteRef)
	}
	canonicalForArgs := func(args []string) string {
		for _, arg := range args {
			ref := strings.TrimPrefix(arg, "--ref=")
			if !strings.HasPrefix(ref, pollNotesNamespace+"/") {
				continue
			}
			switch {
			case strings.HasSuffix(ref, "/review-request"):
				return reviewRequestNoteRef
			case strings.HasSuffix(ref, "/checks"):
				return checksNoteRef
			case strings.HasSuffix(ref, "/verdict"):
				return verdictNoteRef
			}
		}
		return ""
	}
	advertisedOID := func(ref string) string {
		for _, line := range strings.Split(strings.TrimSpace(tc.noteRefs), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[1] == ref {
				return fields[0]
			}
		}
		return ""
	}

	p := &Poller{Root: t.TempDir()}
	p.Run = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "git" {
			t.Fatalf("unexpected tool %q", name)
		}
		switch {
		case slices.Contains(args, "refs/heads/forest/*"):
			return []byte(tc.branches), tc.branchErr
		case slices.Contains(args, "refs/heads/forest/4-work"):
			if !allNoteRefs(args) {
				t.Fatalf("final confirmation omits note refs: %v", args)
			}
			if !completedReads[reviewRequestNoteRef] || !completedReads[verdictNoteRef] {
				t.Fatalf("final confirmation preceded selected note reads: %v", completedReads)
			}
			branch := tc.reAdvertised
			if branch == "" {
				branch = tc.branches
			}
			notes := tc.reAdvertisedNotes
			if notes == "" {
				notes = tc.noteRefs
			}
			return []byte(branch + notes), tc.reAdvertiseErr
		case slices.Contains(args, "ls-remote") && allNoteRefs(args):
			return []byte(tc.noteRefs), tc.fetchErr
		case slices.Contains(args, "fetch"):
			for canonical := range map[string]struct{}{reviewRequestNoteRef: {}, checksNoteRef: {}, verdictNoteRef: {}} {
				prefix := canonical + ":"
				for _, arg := range args {
					if strings.HasPrefix(arg, prefix) {
						private := strings.TrimPrefix(arg, prefix)
						if !strings.HasPrefix(private, pollNotesNamespace+"/") {
							t.Fatalf("fetch destination is not private: %v", args)
						}
						privateCanonical[private] = canonical
						return nil, tc.actualFetchErr
					}
				}
			}
			t.Fatalf("fetch does not use a canonical-to-private refspec: %v", args)
			return nil, nil
		case slices.Contains(args, "rev-parse"):
			canonical := privateCanonical[args[len(args)-1]]
			oid := advertisedOID(canonical)
			if tc.fetchedOID != "" {
				oid = tc.fetchedOID
			}
			return []byte(oid + "\n"), tc.revParseErr
		case slices.Contains(args, "update-ref"):
			ref := args[len(args)-1]
			if !slices.Contains(args, "-d") || !strings.HasPrefix(ref, pollNotesNamespace+"/") {
				t.Fatalf("Poll deleted non-private ref: %v", args)
			}
			if tc.deletedRefs != nil {
				*tc.deletedRefs = append(*tc.deletedRefs, ref)
			}
			return nil, nil
		case slices.Contains(args, "show"):
			canonical := canonicalForArgs(args)
			readTargets[canonical] = args[len(args)-1]
			switch canonical {
			case reviewRequestNoteRef:
				if tc.reviewErr != nil {
					return nil, tc.reviewErr
				}
				if tc.missingReview {
					completedReads[canonical] = true
					return nil, gitExitError(t, 1)
				}
				return []byte(tc.review), nil
			case verdictNoteRef:
				if tc.verdictErr != nil {
					return nil, tc.verdictErr
				}
				if tc.missingVerdict {
					completedReads[canonical] = true
					return nil, gitExitError(t, 1)
				}
				return []byte(tc.verdict), nil
			default:
				t.Fatalf("unexpected note ref in show: %v", args)
				return nil, nil
			}
		case slices.Contains(args, "ls-tree"):
			if tc.treeErr != nil {
				return nil, tc.treeErr
			}
			if tc.notePaths != "" {
				return []byte(tc.notePaths), nil
			}
			target := tc.noteTarget
			if target == "" {
				target = strings.Fields(tc.branches)[0]
			}
			return []byte(target[:2] + "/" + target[2:] + "\n"), nil
		case slices.Contains(args, "log"):
			if tc.logErr != nil {
				return nil, tc.logErr
			}
			canonical := canonicalForArgs(args)
			target := readTargets[canonical]
			if !slices.Contains(args, "--") || canonical == "" || !isSHA(target) || args[len(args)-1] != target[:2]+"/"+target[2:] {
				t.Fatalf("identity lookup does not use the exact private-ref target path: %v", args)
			}
			completedReads[canonical] = true
			if canonical == reviewRequestNoteRef {
				if tc.reviewIdentity != "" {
					return []byte(tc.reviewIdentity), nil
				}
				return []byte("Iron Forest Builder\x00builder@forest.invalid\n"), nil
			}
			if tc.verdictIdentity != "" {
				return []byte(tc.verdictIdentity), nil
			}
			return []byte("Iron Forest Verifier\x00verifier@forest.invalid\n"), nil
		default:
			t.Fatalf("unexpected git args: %v", args)
			return nil, nil
		}
	}
	return p
}

func assertPollPrivateCleanup(t *testing.T, tc notePollCase, refs []string) {
	t.Helper()
	want := tc.branchErr == nil && strings.Contains(tc.branches, " refs/heads/forest/4-work")
	if !want {
		if len(refs) != 0 {
			t.Fatalf("deleted refs=%v want none", refs)
		}
		return
	}
	if len(refs) != 3 {
		t.Fatalf("deleted refs=%v want 3 private refs", refs)
	}
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if !strings.HasPrefix(ref, pollNotesNamespace+"/") || seen[ref] {
			t.Fatalf("deleted refs are not unique private refs: %v", refs)
		}
		seen[ref] = true
	}
}

func TestPollTransportErrorsPreserveObservableIdentity(t *testing.T) {
	sha := strings.Repeat("d", 40)
	sentinel := errors.New("transport sentinel")
	cases := []struct {
		name string
		call func(*Poller) error
	}{
		{name: "branch lookup", call: func(p *Poller) error {
			_, err := p.branchTips(context.Background())
			return err
		}},
		{name: "review read", call: func(p *Poller) error {
			_, err := p.coordinationNote(context.Background(), reviewRequestNoteRef, sha, "builder", "fixer")
			return err
		}},
		{name: "verdict read", call: func(p *Poller) error {
			_, err := p.coordinationNote(context.Background(), verdictNoteRef, sha, "verifier")
			return err
		}},
		{name: "final advertisement", call: func(p *Poller) error {
			return p.confirmSnapshot(context.Background(), branchTip{Name: "forest/4-work", SHA: sha}, pollNoteSnapshot{})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poller := &Poller{Root: t.TempDir(), Run: func(context.Context, string, ...string) ([]byte, error) {
				return nil, sentinel
			}}
			if err := tc.call(poller); !errors.Is(err, sentinel) {
				t.Fatalf("error=%v, want transport sentinel", err)
			}
		})
	}

	t.Run("actual fetch", func(t *testing.T) {
		deleted := []string{}
		tc := notePollCase{
			branches:       sha + " refs/heads/forest/4-work\n",
			noteRefs:       sha + " refs/notes/forest/review-request\n",
			actualFetchErr: sentinel,
			deletedRefs:    &deleted,
		}
		_, err := notePoller(t, tc).fetchNotes(context.Background())
		if !errors.Is(err, sentinel) {
			t.Fatalf("error=%v, want transport sentinel", err)
		}
		assertPollPrivateCleanup(t, tc, deleted)
	})
}

func TestVerifierPollMatrix(t *testing.T) {
	sha := strings.Repeat("b", 40)
	validBranch := sha + " refs/heads/forest/4-work\n"
	cases := []notePollCase{
		{name: "empty", want: 1},
		{name: "no review request", branches: validBranch, missingReview: true, want: 1},
		{name: "eligible", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, want: 0},
		{name: "eligible Fixer request", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), reviewIdentity: "Iron Forest Fixer\x00fixer@forest.invalid\n", missingVerdict: true, want: 0},
		{name: "duplicate note paths", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), notePaths: sha + "\n" + sha[:2] + "/" + sha[2:] + "\n", missingVerdict: true, want: 2},
		{name: "branch moved after notes", branches: validBranch, reAdvertised: strings.Repeat("e", 40) + " refs/heads/forest/4-work\n", noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, want: 2},
		{name: "note moved after reads", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reAdvertisedNotes: strings.Repeat("e", 40) + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, want: 2},
		{name: "verdict appeared after reads", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reAdvertisedNotes: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), missingVerdict: true, want: 2},
		{name: "advertised note moved during fetch", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", fetchedOID: strings.Repeat("e", 40), want: 2},
		{name: "malformed branch advertisement", branches: validBranch, reAdvertised: "not a branch\n", noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, want: 2},
		{name: "wrong request branch", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNoteBranch(sha, "forest/4-other"), missingVerdict: true, want: 2},
		{name: "wrong request identity", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), reviewIdentity: "Builder\x00builder@forest.invalid\n", missingVerdict: true, want: 2},
		{name: "padded request author", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), reviewIdentity: " Iron Forest Builder\x00builder@forest.invalid\n", missingVerdict: true, want: 2},
		{name: "wrong verdict identity", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "approve"), verdictIdentity: "Operator\x00operator@example.invalid\n", want: 2},
		{name: "padded verdict email", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "approve"), verdictIdentity: "Iron Forest Verifier\x00verifier@forest.invalid \n", want: 2},
		{name: "wrong note target", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), noteTarget: strings.Repeat("e", 40), missingVerdict: true, want: 2},
		{name: "malformed ref", branches: sha + " refs/heads/forest/bad\n", want: 2},
		{name: "malformed note", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: `{}`, missingVerdict: true, want: 2},
		{name: "post-show tree exit 1", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), treeErr: gitExitError(t, 1), want: 2},
		{name: "post-show log exit 1", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), logErr: gitExitError(t, 1), want: 2},
		{name: "over-capacity review tree", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), notePaths: strings.Join(pollNoteRows(auditorCapacityEntries+1), "\n") + "\n", missingVerdict: true, want: 1},
		{name: "review tree transport overflow", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), treeErr: errTrustedTransportOutputOverflow, missingVerdict: true, want: 1},
		{name: "malformed review tree row", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), notePaths: sha[:2] + "/" + sha[2:] + "\nunexpected target\n", missingVerdict: true, want: 2},
		{name: "outage", branchErr: errors.New("offline"), want: 2},
		{name: "note fetch failure", branches: validBranch, fetchErr: errors.New("fetch failed"), want: 2},
		{name: "actual note fetch failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", actualFetchErr: errors.New("actual fetch failed"), want: 2},
		{name: "fetched note lookup failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", revParseErr: errors.New("fetched note lookup failed"), want: 2},
		{name: "review transport failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reviewErr: errors.New("review read failed"), want: 2},
		{name: "joined canceled note read", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reviewErr: errors.Join(gitExitError(t, 1), context.Canceled), want: 2},
		{name: "recursively joined missing note", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reviewErr: errors.Join(errors.Join(gitExitError(t, 1))), want: 1},
		{name: "joined cleanup note read", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reviewErr: errors.Join(gitExitError(t, 1), errors.New("cleanup failed")), want: 2},
		{name: "joined duplicate exit errors", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", reviewErr: errors.Join(gitExitError(t, 1), gitExitError(t, 1)), want: 2},
		{name: "verdict transport failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdictErr: errors.New("verdict read failed"), want: 2},
		{name: "final advertisement failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n", review: pollReviewNote(sha), missingVerdict: true, reAdvertiseErr: errors.New("re-advertisement failed"), want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deleted := []string{}
			tc.deletedRefs = &deleted
			if got := notePoller(t, tc).verifier(context.Background()); got != tc.want {
				t.Fatalf("poll exit=%d want %d", got, tc.want)
			}
			assertPollPrivateCleanup(t, tc, deleted)
		})
	}
}

func TestFixerPollMatrix(t *testing.T) {
	sha := strings.Repeat("c", 40)
	validBranch := sha + " refs/heads/forest/4-work\n"
	cases := []notePollCase{
		{name: "empty", want: 1},
		{name: "no verdict", branches: validBranch, missingVerdict: true, want: 1},
		{name: "eligible", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), want: 0},
		{name: "eligible Fixer request", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), reviewIdentity: "Iron Forest Fixer\x00fixer@forest.invalid\n", verdict: pollVerdictNote(sha, "changes"), want: 0},
		{name: "branch moved after notes", branches: validBranch, reAdvertised: strings.Repeat("e", 40) + " refs/heads/forest/4-work\n", noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), want: 2},
		{name: "malformed branch advertisement", branches: validBranch, reAdvertised: "not a branch\n", noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), want: 2},
		{name: "wrong request branch", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNoteBranch(sha, "forest/4-other"), verdict: pollVerdictNote(sha, "changes"), want: 2},
		{name: "wrong request identity", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), reviewIdentity: "Builder\x00builder@forest.invalid\n", want: 2},
		{name: "padded request email", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), reviewIdentity: "Iron Forest Builder\x00builder@forest.invalid \n", want: 2},
		{name: "wrong verdict identity", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), verdictIdentity: "Operator\x00operator@example.invalid\n", want: 2},
		{name: "padded verdict author", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), verdictIdentity: " Iron Forest Verifier\x00verifier@forest.invalid\n", want: 2},
		{name: "wrong note target", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), noteTarget: strings.Repeat("e", 40), want: 2},
		{name: "malformed ref", branches: sha + " refs/heads/forest/bad\n", want: 2},
		{name: "malformed note", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: `{}`, want: 2},
		{name: "post-show tree exit 1", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), treeErr: gitExitError(t, 1), want: 2},
		{name: "over-capacity verdict tree", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), notePaths: strings.Join(pollNoteRows(auditorCapacityEntries+1), "\n") + "\n", want: 1},
		{name: "verdict tree transport overflow", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), treeErr: errTrustedTransportOutputOverflow, want: 1},
		{name: "malformed verdict tree row", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdict: pollVerdictNote(sha, "changes"), notePaths: sha[:2] + "/" + sha[2:] + "\nunexpected target\n", want: 2},
		{name: "absent remote ref stays local", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), want: 0},
		{name: "outage", branchErr: errors.New("offline"), want: 2},
		{name: "note fetch failure", branches: validBranch, fetchErr: errors.New("fetch failed"), want: 2},
		{name: "actual note fetch failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", actualFetchErr: errors.New("actual fetch failed"), want: 2},
		{name: "fetched note lookup failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", revParseErr: errors.New("fetched note lookup failed"), want: 2},
		{name: "review transport failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", reviewErr: errors.New("review read failed"), verdict: pollVerdictNote(sha, "changes"), want: 2},
		{name: "verdict transport failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/verdict\n", verdictErr: errors.New("verdict read failed"), want: 2},
		{name: "final advertisement failure", branches: validBranch, noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n", review: pollReviewNote(sha), verdict: pollVerdictNote(sha, "changes"), reAdvertiseErr: errors.New("re-advertisement failed"), want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deleted := []string{}
			tc.deletedRefs = &deleted
			if got := notePoller(t, tc).fixer(context.Background()); got != tc.want {
				t.Fatalf("poll exit=%d want %d", got, tc.want)
			}
			assertPollPrivateCleanup(t, tc, deleted)
		})
	}
}

func TestPollPrivateCleanupUsesFreshContext(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, failCleanup := range []bool{false, true} {
		name := "success"
		want := 1
		if failCleanup {
			name = "cleanup error"
			want = 2
		}
		t.Run(name, func(t *testing.T) {
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			poller := notePoller(t, notePollCase{
				branches: sha + " refs/heads/forest/4-work\n",
				noteRefs: sha + " refs/notes/forest/review-request\n" + sha + " refs/notes/forest/verdict\n",
				review:   pollReviewNote(sha),
				verdict:  pollVerdictNote(sha, "approve"),
			})
			run := poller.Run
			reads := 0
			cleanupCalls := 0
			cleanupErr := errors.New("cleanup failed")
			poller.Run = func(ctx context.Context, tool string, args ...string) ([]byte, error) {
				if slices.Contains(args, "update-ref") {
					if err := ctx.Err(); err != nil {
						t.Fatalf("cleanup context is already done: %v", err)
					}
					if parent.Err() != context.Canceled {
						t.Fatalf("Poll parent error=%v want canceled", parent.Err())
					}
					deadline, ok := ctx.Deadline()
					if !ok {
						t.Fatal("cleanup context has no deadline")
					}
					if remaining := time.Until(deadline); remaining <= 0 || remaining > time.Second {
						t.Fatalf("cleanup deadline remaining=%v want within one second", remaining)
					}
					cleanupCalls++
					if failCleanup && cleanupCalls == 1 {
						return nil, cleanupErr
					}
				}
				output, err := run(ctx, tool, args...)
				if err == nil && slices.Contains(args, "log") {
					reads++
					if reads == 2 {
						cancel()
					}
				}
				return output, err
			}
			if got := poller.verifier(parent); got != want {
				t.Fatalf("poll exit=%d want %d", got, want)
			}
			if reads != 2 {
				t.Fatalf("completed note reads=%d want 2", reads)
			}
			if cleanupCalls != 3 {
				t.Fatalf("cleanup calls=%d want 3", cleanupCalls)
			}
		})
	}
}

func pollChecksNote(sha, result string) string {
	return `{"schema":"forest.checks.v1","revision":"` + sha + `","results":[` + result + `],"time":"2026-08-10T00:00:00Z"}`
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
		{name: "review", data: validReview, keys: []string{"schema", "issue", "branch", "revision", "time"}, decode: decodeReviewNote},
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

	missingReview := `{"schema":"forest.review-request.v1","issue":4,"branch":"forest/4-work","revision":"` + sha + `"}`
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
		{name: "review duplicate member", data: `{"schema":"forest.review-request.v1","schema":"forest.review-request.v1","issue":4,"branch":"forest/4-work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`, decode: decodeReviewNote},
		{name: "checks duplicate nested member", data: pollChecksNote(sha, `{"name":"test","name":"test","ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "verdict duplicate member", data: `{"schema":"forest.verdict.v1","revision":"` + sha + `","verdict":"approve","summary":"done","summary":"done","time":"2026-08-10T00:00:00Z"}`, decode: decodeVerdictNote},
		{name: "review mixed-case duplicate", data: strings.Replace(validReview, `"schema":"forest.review-request.v1"`, `"schema":"bad","Schema":"forest.review-request.v1"`, 1), decode: decodeReviewNote},
		{name: "check mixed-case duplicate", data: pollChecksNote(sha, `{"name":"bad","Name":"test","ok":true,"exit":0}`), decode: decodeChecksNote},
		{name: "trailing JSON", data: validReview + ` {}`, decode: decodeReviewNote},
		{name: "review malformed time", data: strings.Replace(validReview, "2026-08-10T00:00:00Z", "not-a-time", 1), decode: decodeReviewNote},
		{name: "checks malformed time", data: strings.Replace(validChecks, "2026-08-10T00:00:00Z", "not-a-time", 1), decode: decodeChecksNote},
		{name: "verdict malformed time", data: strings.Replace(validVerdict, "2026-08-10T00:00:00Z", "not-a-time", 1), decode: decodeVerdictNote},
		{name: "verdict blank summary", data: strings.Replace(validVerdict, `"summary":"done"`, `"summary":"  "`, 1), decode: decodeVerdictNote},
		{name: "review empty branch slug", data: pollReviewNoteBranch(sha, "forest/4-"), decode: decodeReviewNote},
		{name: "review uppercase branch slug", data: pollReviewNoteBranch(sha, "forest/4-Bad"), decode: decodeReviewNote},
		{name: "review underscored branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad_name"), decode: decodeReviewNote},
		{name: "review leading branch separator", data: pollReviewNoteBranch(sha, "forest/4--bad"), decode: decodeReviewNote},
		{name: "review repeated branch separator", data: pollReviewNoteBranch(sha, "forest/4-bad--name"), decode: decodeReviewNote},
		{name: "review trailing branch separator", data: pollReviewNoteBranch(sha, "forest/4-bad-"), decode: decodeReviewNote},
		{name: "review spaced branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad name"), decode: decodeReviewNote},
		{name: "review dotted branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad.name"), decode: decodeReviewNote},
		{name: "review leading dot branch", data: pollReviewNoteBranch(sha, "forest/4-.bad"), decode: decodeReviewNote},
		{name: "review dot sequence branch", data: pollReviewNoteBranch(sha, "forest/4-bad..name"), decode: decodeReviewNote},
		{name: "review nested branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad/name"), decode: decodeReviewNote},
		{name: "review tilde branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad~name"), decode: decodeReviewNote},
		{name: "review caret branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad^name"), decode: decodeReviewNote},
		{name: "review colon branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad:name"), decode: decodeReviewNote},
		{name: "review question branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad?name"), decode: decodeReviewNote},
		{name: "review asterisk branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad*name"), decode: decodeReviewNote},
		{name: "review bracket branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad[name"), decode: decodeReviewNote},
		{name: "review single-at branch slug", data: pollReviewNoteBranch(sha, "forest/4-@"), decode: decodeReviewNote},
		{name: "review backslash branch slug", data: strings.Replace(validReview, "forest/4-work", `forest/4-bad\\name`, 1), decode: decodeReviewNote},
		{name: "review control branch slug", data: strings.Replace(validReview, "forest/4-work", `forest/4-bad\u0001name`, 1), decode: decodeReviewNote},
		{name: "review at-brace branch slug", data: pollReviewNoteBranch(sha, "forest/4-bad@{name"), decode: decodeReviewNote},
		{name: "review trailing dot branch", data: pollReviewNoteBranch(sha, "forest/4-bad."), decode: decodeReviewNote},
		{name: "review lock suffix branch", data: pollReviewNoteBranch(sha, "forest/4-bad.lock"), decode: decodeReviewNote},
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

func TestPollEnumerationSkipLogsExplicitLine(t *testing.T) {
	sha := strings.Repeat("f", 40)
	poller := &Poller{Root: t.TempDir(), Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "ls-tree") {
			return nil, errTrustedTransportOutputOverflow
		}
		return []byte("payload\n"), nil
	}}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = writer
	_, err = poller.coordinationNote(context.Background(), reviewRequestNoteRef, sha, "builder", "fixer")
	os.Stderr = old
	_ = writer.Close()
	logged, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !errors.Is(err, pollEnumerationSkip) {
		t.Fatalf("coordinationNote err=%v, want pollEnumerationSkip", err)
	}
	line := string(logged)
	if !strings.Contains(line, reviewRequestNoteRef) || !strings.Contains(line, "no work") {
		t.Fatalf("log line %q does not name the canonical ref and inline the no-work skip", line)
	}
}

// pollNoteRows returns count distinct valid flat (unslashed) revision paths.
func pollNoteRows(count int) []string {
	rows := make([]string, 0, count)
	for index := range count {
		rows = append(rows, fmt.Sprintf("%040x", index))
	}
	return rows
}

// buildPollNotesRef writes, commits, and pushes a notes tree for ref. The row
// matching target holds payload; every other row holds a shared dummy blob.
func buildPollNotesRef(t *testing.T, root, ref string, rows []string, target, payload, name, email string) {
	t.Helper()
	sorted := append([]string(nil), rows...)
	sort.Strings(sorted)
	blobFor := func(content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "note")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(runGitDir(t, root, "hash-object", "-w", path)))
	}
	targetBlob := blobFor(payload)
	dummyBlob := blobFor("noise\n")
	var input strings.Builder
	targetSeen := false
	for _, row := range sorted {
		if row == target {
			targetSeen = true
			fmt.Fprintf(&input, "100644 blob %s\t%s\n", targetBlob, row)
			continue
		}
		fmt.Fprintf(&input, "100644 blob %s\t%s\n", dummyBlob, row)
	}
	if !targetSeen {
		fmt.Fprintf(&input, "100644 blob %s\t%s\n", targetBlob, target)
	}
	command := exec.Command("git", "-C", root, "mktree")
	command.Stdin = strings.NewReader(input.String())
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("mktree: %v\n%s", err, output)
	}
	tree := strings.TrimSpace(string(output))
	commit := strings.TrimSpace(string(runGitDir(t, root, "-c", "user.name="+name, "-c", "user.email="+email, "commit-tree", tree, "-m", "poll notes")))
	runGitDir(t, root, "update-ref", ref, commit)
	runGitDir(t, root, "push", "--force", "origin", ref+":"+ref)
}

func TestPollEnumerationCapacityRealNotes(t *testing.T) {
	root, _ := testClone(t)
	runGitDir(t, root, "checkout", "-b", "forest/4-work")
	if err := os.WriteFile(filepath.Join(root, "poll-change"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "poll-change")
	runGitDir(t, root, "commit", "-m", "poll change")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/forest/4-work")
	targetPath := tip

	runPoll := func(want int, label string) {
		t.Helper()
		if got := NewPoller(root, "owner/name").verifier(context.Background()); got != want {
			t.Fatalf("%s poll exit=%d want %d", label, got, want)
		}
		if refs := strings.TrimSpace(string(runGitDir(t, root, "for-each-ref", "--format=%(refname)", pollNotesNamespace+"/"))); refs != "" {
			t.Fatalf("%s left private Poll refs behind: %s", label, refs)
		}
	}

	buildPollNotesRef(t, root, reviewRequestNoteRef, []string{targetPath}, targetPath, pollReviewNote(tip), "Iron Forest Builder", "builder@forest.invalid")
	runPoll(0, "small tree")

	over := pollNoteRows(auditorCapacityEntries + 1)
	over = append(over, targetPath)
	buildPollNotesRef(t, root, reviewRequestNoteRef, over, targetPath, pollReviewNote(tip), "Iron Forest Builder", "builder@forest.invalid")
	runPoll(1, "over-capacity")

	buildPollNotesRef(t, root, reviewRequestNoteRef, []string{targetPath, "unexpected target"}, targetPath, pollReviewNote(tip), "Iron Forest Builder", "builder@forest.invalid")
	runPoll(2, "malformed row")

	overflow := pollNoteRows((trustedTransportOutputLimit / 41) + 500)
	overflow = append(overflow, targetPath)
	buildPollNotesRef(t, root, reviewRequestNoteRef, overflow, targetPath, pollReviewNote(tip), "Iron Forest Builder", "builder@forest.invalid")
	runPoll(1, "transport overflow")
}
