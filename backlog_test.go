package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSubjectSetReportsContentionAndIdle(t *testing.T) {
	set := newSubjectSet()
	if !set.idle() {
		t.Fatal("new subject set must be idle")
	}
	if !set.claim("branch-forest/7-fix") {
		t.Fatal("first subject claim must succeed")
	}
	if set.idle() {
		t.Fatal("subject set with an active key must not be idle")
	}
	if set.claim("branch-forest/7-fix") {
		t.Fatal("second subject claim must be refused")
	}
	set.release("branch-forest/7-fix")
	if !set.idle() || !set.claim("branch-forest/7-fix") {
		t.Fatal("released key must become available")
	}
	set.release("branch-forest/7-fix")
}

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

func TestEligibleItemsExcludesLabelsAndCoveredBranches(t *testing.T) {
	labelled := Item{ID: "2"}
	labelled.Tags = append(labelled.Tags, "parked")
	items := []Item{{ID: "1", Title: "ready"}, labelled, {ID: "3", Title: "covered"}}
	got := eligibleFrom(items, []string{"forest/3-covered"}, []string{"parked"}, nil)
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
	got := eligibleFrom(items, nil, []string{"forest:failed"}, []string{"forest:ready"})
	if len(got) != 1 || got[0].ID != "2" {
		t.Fatalf("opt-in eligible items = %+v, want only item 2", got)
	}
}

// trackerStub is a Tracker that always returns one opaque, non-numeric item so
// tests can drive the controller through the port without the host CLI.
type trackerStub struct{ items []Item }

func (t trackerStub) ListOpen() ([]Item, error)                   { return t.items, nil }
func (trackerStub) ListByTag(tag string) ([]Item, error)          { return nil, nil }
func (trackerStub) Get(id string) (Item, error)                   { return Item{ID: id}, nil }
func (trackerStub) Comment(id, body string) error                 { return nil }
func (trackerStub) Close(id string) error                         { return nil }
func (trackerStub) SetTags(id string, add, remove []string) error { return nil }

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
}

// TestBranchSelectRecoversOpaqueID proves the verifier selector derives the
// Subject's opaque id from an existing forest branch that was built for a
// non-numeric tracker id, including one that itself contains the '-' delimiter.
func TestBranchSelectRecoversOpaqueID(t *testing.T) {
	_, work, _ := notesTestRepository(t)
	const id = "hab-01J9X"
	branch := "forest/" + encodeBranchID(id) + "-slug"
	notesTestGit(t, work, "checkout", "-q", "-b", branch)
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	notesTestGit(t, work, "commit", "-qam", "branch work")
	notesTestGit(t, work, "push", "-q", "-u", "origin", branch)

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
