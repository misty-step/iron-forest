package main

import (
	"errors"
	"os"
	"path/filepath"
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

func TestEligibleItemsExcludesLabelsAndCoveredBranches(t *testing.T) {
	labelled := Item{ID: "2"}
	labelled.Tags = append(labelled.Tags, "parked")
	items := []Item{{ID: "1", Title: "ready"}, labelled, {ID: "3", Title: "covered"}, {ID: "4", Title: "retiring"}}
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

func TestBuilderSelectRejectsInvalidIdentity(t *testing.T) {
	const secret = "sk-AAAAAAAAAAAAAAAA"
	for _, tc := range []struct {
		name string
		item Item
		want string
	}{
		{name: "empty id", item: Item{UpdatedAt: "revision"}, want: "empty"},
		{name: "credential-shaped id", item: Item{ID: secret, UpdatedAt: "revision"}, want: "credential-shaped"},
		{name: "credential-shaped revision", item: Item{ID: "9", UpdatedAt: secret}, want: "credential-shaped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := trackerFor
			trackerFor = func(string) Tracker { return trackerStub{items: []Item{tc.item}} }
			defer func() { trackerFor = old }()

			_, err := (builderFlow{}).Select(defaultConfig(), t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid %s was accepted: %v", tc.name, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("selection error echoed rejected identity: %v", err)
			}
		})
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

	old := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = old }()

	out, err := (fixerFlow{}).Act(defaultConfig(), t.TempDir(), Subject{
		ID: "9", Branch: "forest/9", Revision: "revision",
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
