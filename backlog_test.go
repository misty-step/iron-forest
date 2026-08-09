package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLedgerReadsLegacyIntegerAndOpaqueStringIssue pins the decided
// compatibility shape: the ledger's `issue` field is now an opaque string, but
// the loader still reads rows written before the migration that carry an
// integer, so an append-only ledger never breaks the report.
func TestLedgerReadsLegacyIntegerAndOpaqueStringIssue(t *testing.T) {
	dir := t.TempDir()
	oldRow := `{"time":"t","flow":"builder","subject":"item-69","revision":"r","issue":69}`
	newRow := `{"time":"t","flow":"builder","subject":"item-hab_01J9X","revision":"r","issue":"hab_01J9X"}`
	if err := os.WriteFile(filepath.Join(dir, "runs.jsonl"), []byte(oldRow+"\n"+newRow+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runs, invalid, err := loadLedger(filepath.Join(dir, "runs.jsonl"))
	if err != nil {
		t.Fatalf("loadLedger: %v", err)
	}
	if invalid != 0 {
		t.Fatalf("invalid rows = %d, want 0", invalid)
	}
	if len(runs) != 2 {
		t.Fatalf("loaded %d rows, want 2", len(runs))
	}
	if runs[0].ID != "69" {
		t.Errorf("legacy integer row id = %q, want 69", runs[0].ID)
	}
	if runs[1].ID != "hab_01J9X" {
		t.Errorf("opaque string row id = %q, want hab_01J9X", runs[1].ID)
	}
}
func TestAppendRunRedactsError(t *testing.T) {
	const secret = "sk-AAAAAAAAAAAAAAAA"
	dir := t.TempDir()
	if err := appendRun(dir, runRecord{Flow: "builder", Status: "failed", Error: "provider rejected " + secret}); err != nil {
		t.Fatal(err)
	}
	rows, invalid, err := loadLedger(filepath.Join(dir, "runs.jsonl"))
	if err != nil || invalid != 0 || len(rows) != 1 {
		t.Fatalf("load redacted Ledger = (%#v, %d, %v)", rows, invalid, err)
	}
	if strings.Contains(rows[0].Error, secret) || !strings.Contains(rows[0].Error, secretRedacted) {
		t.Fatalf("Ledger error = %q, want marker without original", rows[0].Error)
	}
}

func TestLedgerWritesReviewVerdictUnderReviewKey(t *testing.T) {
	dir := t.TempDir()
	if err := appendRun(dir, runRecord{
		Flow: "verifier", Status: "reviewed", ReviewVerdict: "approve",
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "runs.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	row := string(body)
	if !strings.Contains(row, `"review":"approve"`) || strings.Contains(row, `"verdict"`) {
		t.Fatalf("Ledger row = %s, want review key without verdict alias", row)
	}
}

func TestGitHubTrackerCloseAcceptsAlreadyClosedItem(t *testing.T) {
	closeErr := errors.New("Host reported already closed")
	old := ghJSON
	defer func() { ghJSON = old }()
	var calls [][]string
	ghJSON = func(args ...string) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch {
		case len(args) > 1 && args[1] == "close":
			return nil, closeErr
		case len(args) > 1 && args[1] == "view":
			return []byte(`{"state":"CLOSED"}`), nil
		default:
			return nil, errors.New("unexpected Host command")
		}
	}
	if err := (githubTracker{repo: "owner/repo"}).Close("9"); err != nil {
		t.Fatalf("retry close of closed Item = %v, want success", err)
	}
	if len(calls) != 2 ||
		!hasArgumentPair(calls[1], "--json", "state") {
		t.Fatalf("close recovery calls = %v, want close then exact state read", calls)
	}
}
func TestGitHubTrackerListRejectsNullAndRequestsAllItems(t *testing.T) {
	old := ghJSON
	defer func() { ghJSON = old }()
	var got []string
	ghJSON = func(args ...string) ([]byte, error) {
		got = append([]string(nil), args...)
		return []byte("null"), nil
	}
	_, err := (githubTracker{repo: "owner/repo"}).ListOpen()
	if err == nil || !strings.Contains(err.Error(), "null") {
		t.Fatalf("null Tracker list = %v, want refusal", err)
	}
	if !hasArgumentPair(got, "--limit", strconv.Itoa(int(^uint(0)>>1))) {
		t.Fatalf("Tracker list args = %v, want all-item limit", got)
	}
}
func TestGitHubTrackerClassifiesTransportAndMalformedEvidence(t *testing.T) {
	old := ghJSON
	defer func() { ghJSON = old }()
	ghJSON = func(...string) ([]byte, error) {
		return nil, errors.New("temporary network failure")
	}
	if _, err := (githubTracker{repo: "owner/repo"}).ListOpen(); !errors.Is(err, errTrackerUnavailable) {
		t.Fatalf("transport error = %v, want retryable Tracker classification", err)
	}
	ghJSON = func(...string) ([]byte, error) {
		return []byte(`{"not":"an array"}`), nil
	}
	if _, err := (githubTracker{repo: "owner/repo"}).ListOpen(); !errors.Is(err, errTrackerEvidenceInvalid) {
		t.Fatalf("malformed response = %v, want invalid Tracker evidence", err)
	}
}
func TestGitHubTrackerCloseRequiresExactStateEvidence(t *testing.T) {
	old := ghJSON
	defer func() { ghJSON = old }()
	for _, body := range []string{`malformed`, `{}`, `null`, `{"state":"unknown"}`} {
		ghJSON = func(args ...string) ([]byte, error) {
			if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
				return nil, errors.New("response lost")
			}
			return []byte(body), nil
		}
		if err := (githubTracker{repo: "owner/repo"}).Close("9"); !errors.Is(err, errTrackerEvidenceInvalid) {
			t.Fatalf("close evidence %q = %v, want invalid evidence", body, err)
		}
	}
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			return nil, errors.New("response lost")
		}
		return []byte(`{"state":"OPEN"}`), nil
	}
	err := (githubTracker{repo: "owner/repo"}).Close("9")
	if !errors.Is(err, errTrackerEffectNotApplied) || !errors.Is(err, errTrackerUnavailable) {
		t.Fatalf("open close evidence = %v, want known-unapplied retry classification", err)
	}
	ghJSON = func(args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "issue" && args[1] == "close" {
			return nil, errors.New("response lost")
		}
		return []byte(`{"state":"CLOSED"}`), nil
	}
	if err := (githubTracker{repo: "owner/repo"}).Close("9"); err != nil {
		t.Fatalf("closed reconciliation = %v, want success", err)
	}
}

func TestEligibleItemsExcludesLabelsAndCoveredBranches(t *testing.T) {
	labelled := Item{ID: "2", Tags: []string{"parked"}}
	failed := Item{ID: "5", Tags: []string{failedLabel}}
	items := []Item{
		{ID: "1", Title: "ready"}, labelled, failed,
		{ID: "3", Title: "covered"}, {ID: "4", Title: "retiring"},
	}
	got := eligibleFrom(items, []string{"forest/3-covered"}, []string{"4"}, []string{"parked"}, nil)
	if len(got) != 1 || got[0].ID != "1" {
		t.Fatalf("eligible items = %+v, want only item 1", got)
	}
}

func TestEligibleItemsRequireLabelsOptIn(t *testing.T) {
	label := func(id string, names ...string) Item {
		it := Item{ID: id}
		it.Tags = append(it.Tags, names...)
		return it
	}
	unlabelled := label("1")
	promoted := label("2", "forest:ready")
	braked := label("3", "forest:failed", "forest:ready")
	items := []Item{unlabelled, promoted, braked}

	// With require_labels declared, selection is an opt-in: only an open item
	// carrying the required label is selected. ExcludeLabels composes with the
	// opt-in, so an item that also carries the excluded label is not selected.
	got := eligibleFrom(items, nil, nil, []string{"forest:failed"}, []string{"forest:ready"})
	if len(got) != 1 || got[0].ID != "2" {
		t.Fatalf("opt-in eligible items = %+v, want only item 2", got)
	}
}

// trackerStub is a Tracker that always returns one opaque, non-numeric item so
// tests can drive the controller through the port without the host CLI.
type trackerStub struct{ items []Item }

func (t trackerStub) ListOpen() ([]Item, error)                   { return t.items, nil }
func (trackerStub) Get(id string) (Item, error)                   { return Item{ID: id}, nil }
func (trackerStub) Comment(id, body string) error                 { return nil }
func (trackerStub) Close(id string) error                         { return nil }
func (trackerStub) SetTags(id string, add, remove []string) error { return nil }

type mismatchTracker struct {
	gets   int
	writes int
}

func (t *mismatchTracker) ListOpen() ([]Item, error) { return nil, nil }
func (t *mismatchTracker) Get(string) (Item, error) {
	t.gets++
	return Item{ID: "other", UpdatedAt: "revision"}, nil
}
func (t *mismatchTracker) Comment(string, string) error {
	t.writes++
	return nil
}
func (t *mismatchTracker) Close(string) error {
	t.writes++
	return nil
}
func (t *mismatchTracker) SetTags(string, []string, []string) error {
	t.writes++
	return nil
}

// TestBuilderSelectCarriesOpaqueID proves the controller path — not just the
// worktree helpers — carries a non-numeric tracker id: builderFlow.Select reads
// the tracker through the Tracker port and emits a Subject whose ID is the
// native opaque string unchanged, with no Atoi anywhere in the recovery.
func TestBuilderSelectCarriesOpaqueID(t *testing.T) {
	old := trackerFor
	trackerFor = func(repo string) Tracker {
		return trackerStub{items: []Item{{ID: "hab_01J9X", Title: "opaque", UpdatedAt: "r",
			Tags: []string{"forest:ready"}}}}
	}
	defer func() { trackerFor = old }()

	_, work, _ := notesTestRepository(t)
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Flows.Builder.RequireLabels = []string{"forest:ready"}
	cfg.Flows.Builder.ExcludeLabels = nil

	subjects, err := builderFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 {
		t.Fatalf("builder selected %d subjects, want 1", len(subjects))
	}
	s := subjects[0]
	if s.ID != "hab_01J9X" {
		t.Fatalf("subject ID = %q, want the opaque id hab_01J9X", s.ID)
	}
	if s.Key != "item-hab_01J9X" {
		t.Fatalf("subject key = %q, want item-hab_01J9X", s.Key)
	}
	// The derived branch round-trips back to the same opaque id.
	if got := itemIDFromBranch("forest/" + encodeBranchID(s.ID) + "-opaque"); got != s.ID {
		t.Fatalf("itemIDFromBranch = %q, want %q", got, s.ID)
	}
	for range stalledRunLimit {
		if err := recordStalled(work, "builder", s.Key, s.Revision); err != nil {
			t.Fatal(err)
		}
	}
	subjects, err = builderFlow{}.Select(cfg, work)
	if err != nil || len(subjects) != 0 {
		t.Fatalf("builder selected stalled Subject = (%#v, %v), want none", subjects, err)
	}
}
func TestForestBranchesRejectsNonCanonicalOpaqueIdentity(t *testing.T) {
	remote, work, revision := notesTestRepository(t)
	runGitTest(t, remote, "update-ref", "refs/heads/forest/a%2db-change", revision)
	if branches, err := forestBranches(work); err == nil {
		t.Fatalf("non-canonical branches = %v, want refusal", branches)
	}
}

func TestBuilderSelectCarriesInvalidIdentityAsOneFailure(t *testing.T) {
	const secret = "sk-AAAAAAAAAAAAAAAA"
	for _, tc := range []struct {
		name string
		item Item
		want string
	}{
		{name: "empty id", item: Item{UpdatedAt: "revision"}, want: "empty"},
		{name: "credential-shaped id", item: Item{ID: secret, UpdatedAt: "revision"}, want: "credential-shaped"},
		{name: "credential-shaped revision", item: Item{ID: "9", UpdatedAt: secret}, want: "credential-shaped"},
		{name: "empty revision", item: Item{ID: "9"}, want: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := trackerFor
			trackerFor = func(string) Tracker { return trackerStub{items: []Item{tc.item}} }
			defer func() { trackerFor = old }()

			_, repo, _ := notesTestRepository(t)
			subjects, err := (builderFlow{}).Select(defaultConfig(), repo)
			if err != nil || len(subjects) != 1 || subjects[0].Failure == nil ||
				!strings.Contains(subjects[0].Failure.Error(), tc.want) {
				t.Fatalf("invalid %s selection = (%#v, %v), want one named failure", tc.name, subjects, err)
			}
			if strings.Contains(subjects[0].Failure.Error(), secret) {
				t.Fatalf("selection failure echoed rejected identity: %v", subjects[0].Failure)
			}
			out, actErr := (builderFlow{}).Act(defaultConfig(), t.TempDir(), subjects[0], "run")
			if out.Status != "evidence_failed" || actErr == nil {
				t.Fatalf("invalid %s Act = (%#v, %v), want terminal evidence failure", tc.name, out, actErr)
			}
		})
	}
}

func TestBuilderSelectKeepsHealthyItemBesideDuplicateEvidence(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	old := trackerFor
	trackerFor = func(string) Tracker {
		return trackerStub{items: []Item{
			{ID: "9", Title: "duplicate one", UpdatedAt: "r1"},
			{ID: "9", Title: "duplicate two", UpdatedAt: "r2"},
			{ID: "10", Title: "healthy", UpdatedAt: "r3"},
		}}
	}
	defer func() { trackerFor = old }()

	subjects, err := (builderFlow{}).Select(defaultConfig(), repo)
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	healthy := 0
	for _, subject := range subjects {
		if subject.Failure != nil {
			failures++
		}
		if subject.ID == "10" && subject.Failure == nil {
			healthy++
		}
	}
	if failures != 2 || healthy != 1 {
		t.Fatalf("Builder Subjects = %#v, want two quarantined duplicates and one healthy Item", subjects)
	}
}

func TestTrackerGetMismatchStopsBeforeSinks(t *testing.T) {
	const secret = "sk-AAAAAAAAAAAAAAAA"
	tk := &mismatchTracker{}
	if _, err := validatedTrackerItem(tk, secret); err == nil || !strings.Contains(err.Error(), "credential-shaped") {
		t.Fatalf("credential-shaped Tracker Get ID was accepted: %v", err)
	}
	if tk.gets != 0 {
		t.Fatalf("credential-shaped Tracker Get reached %d source reads", tk.gets)
	}

	_, repo, revision := notesTestRepository(t)
	if err := writeChecks(repo, revision, checksNote{Status: "fail"}, testCommitIdentity()); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "branch", "forest/9-change", revision)
	runGitTest(t, repo, "push", "-q", "origin", "forest/9-change")
	old := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = old }()

	out, err := (fixerFlow{}).Act(defaultConfig(), repo, Subject{
		Key: "branch-forest/9-change", ID: "9", Branch: "forest/9-change", Revision: revision,
	}, "mismatch")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched Tracker Item was accepted: %#v, %v", out, err)
	}
	if strings.Contains(err.Error(), "other") {
		t.Fatalf("mismatch error echoed returned identity: %v", err)
	}
	if tk.gets != 1 {
		t.Fatalf("Tracker Get calls = %d, want 1", tk.gets)
	}
	if tk.writes != 0 {
		t.Fatalf("mismatched Tracker Item reached %d Tracker writes", tk.writes)
	}
}

// TestBranchSelectRecoversOpaqueID proves the verifier selector derives the
// Subject's opaque id from an existing forest branch that was built for a
// non-numeric tracker id, including one that itself contains the '-' delimiter.
func TestBranchSelectRecoversOpaqueID(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	const id = "hab-01J9X"
	branch := "forest/" + encodeBranchID(id) + "-slug"
	runGitTest(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "commit", "-qam", "branch work")
	runGitTest(t, work, "push", "-q", "-u", "origin", branch)

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	subjects, err := verifierFlow{}.Select(cfg, work)
	if err != nil {
		t.Fatal(err)
	}
	if len(subjects) != 1 {
		t.Fatalf("verifier selected %d subjects, want 1", len(subjects))
	}
	// The branch encodes the hyphen so the id stays round-trippable.
	if subjects[0].ID != id {
		t.Fatalf("verifier subject ID = %q, want %q", subjects[0].ID, id)
	}
}

func TestDuplicateBranchItemIdentityIsRejectedDuringAct(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	if err := writeChecks(repo, revision, checksNote{Status: "fail"}, testCommitIdentity()); err != nil {
		t.Fatal(err)
	}
	for _, branch := range []string{"forest/9-a", "forest/9-b"} {
		runGitTest(t, repo, "branch", branch, revision)
		runGitTest(t, repo, "push", "-q", "origin", branch)
	}
	if _, err := forestBranches(repo); !errors.Is(err, errTrackerEvidenceInvalid) {
		t.Fatalf("duplicate branch identity = %v, want invalid Tracker evidence", err)
	}
	_, err := (fixerFlow{}).Act(defaultConfig(), repo, Subject{
		Key: "branch-forest/9-a", ID: "9", Branch: "forest/9-a", Revision: revision,
	}, "duplicate")
	if !errors.Is(err, errTrackerEvidenceInvalid) {
		t.Fatalf("Act accepted duplicate branch identity: %v", err)
	}
}

func TestMalformedBranchCoversRecoverableItemIdentity(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	branch := "forest/foo"
	runGitTest(t, repo, "branch", branch, revision)
	runGitTest(t, repo, "push", "-q", "origin", branch)

	snapshot, err := readForestBranchSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Covered) != 1 || snapshot.Covered[0] != branch ||
		len(snapshot.Actionable) != 0 || len(snapshot.Failures) != 1 ||
		snapshot.Failures[0].ID != "foo" {
		t.Fatalf("malformed branch snapshot = %#v, want quarantined Item foo", snapshot)
	}
	if eligible := eligibleFrom([]Item{{ID: "foo"}}, snapshot.Covered, nil, nil, nil); len(eligible) != 0 {
		t.Fatalf("malformed branch left duplicate Item eligible: %#v", eligible)
	}
}

func TestBranchSnapshotQuarantinesDuplicatesWithoutSuppressingHealthyBranch(t *testing.T) {
	_, repo, revision := notesTestRepository(t)
	for _, branch := range []string{"forest/9-a", "forest/9-b", "forest/10-good"} {
		runGitTest(t, repo, "branch", branch, revision)
		runGitTest(t, repo, "push", "-q", "origin", branch)
	}
	snapshot, err := readForestBranchSnapshot(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Covered) != 3 || len(snapshot.Actionable) != 1 ||
		snapshot.Actionable[0] != "forest/10-good" || len(snapshot.Failures) != 2 {
		t.Fatalf("branch snapshot = %#v, want one healthy branch and two quarantined duplicates", snapshot)
	}
	subjects, err := (verifierFlow{}).Select(defaultConfig(), repo)
	if err != nil {
		t.Fatal(err)
	}
	healthy, failures := 0, 0
	for _, subject := range subjects {
		if subject.Branch == "forest/10-good" && subject.Failure == nil {
			healthy++
		}
		if subject.ID == "9" && subject.Failure != nil {
			failures++
		}
	}
	if healthy != 1 || failures != 2 {
		t.Fatalf("Verifier Subjects = %#v, want one healthy and two quarantined duplicates", subjects)
	}
}
