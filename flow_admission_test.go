package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type admissionProcessFlow struct {
	name    string
	ready   io.Writer
	release io.Reader
}

func (f admissionProcessFlow) Name() string                             { return f.name }
func (f admissionProcessFlow) Select(Config, string) ([]Subject, error) { return nil, nil }
func (f admissionProcessFlow) Interval(Config) time.Duration            { return 0 }
func (f admissionProcessFlow) Enabled(Config) bool                      { return true }
func (f admissionProcessFlow) Act(Config, string, Subject, string) (Outcome, error) {
	if _, err := f.ready.Write([]byte{1}); err != nil {
		return Outcome{}, err
	}
	var release [1]byte
	if _, err := io.ReadFull(f.release, release[:]); err != nil {
		return Outcome{}, err
	}
	return Outcome{Status: "done"}, nil
}

type selectionFlow struct {
	subjects []Subject
	acted    *[]string
}

func (f selectionFlow) Name() string                             { return "builder" }
func (f selectionFlow) Select(Config, string) ([]Subject, error) { return f.subjects, nil }
func (f selectionFlow) Interval(Config) time.Duration            { return 0 }
func (f selectionFlow) Enabled(Config) bool                      { return true }
func (f selectionFlow) Act(_ Config, _ string, s Subject, _ string) (Outcome, error) {
	*f.acted = append(*f.acted, s.Key)
	return Outcome{Status: "done"}, nil
}

func TestActOnSubjectAdmissionHelper(t *testing.T) {
	repo := os.Getenv("FOREST_ACT_ADMISSION_HELPER")
	if repo == "" {
		return
	}
	var subject Subject
	if err := json.Unmarshal([]byte(os.Getenv("FOREST_ACT_SUBJECT")), &subject); err != nil {
		t.Fatal(err)
	}
	ready := os.NewFile(3, "ready")
	release := os.NewFile(4, "release")
	if ready == nil || release == nil {
		t.Fatal("missing synchronization pipe")
	}
	defer ready.Close()
	defer release.Close()
	if code := actOnSubject(admissionProcessFlow{
		name: "builder", ready: ready, release: release,
	}, Config{Repo: admissionTestRepo}, repo, subject, nil); code != 0 {
		t.Fatalf("holder Effect code = %d, want success", code)
	}
}

// TestActOnSubjectAdmissionClaimsAcrossItemAndBranch selects the two real
// Subject forms, then proves separate processes cannot both enter Act.
func TestActOnSubjectAdmissionClaimsAcrossItemAndBranch(t *testing.T) {
	remote, repoA, _ := notesTestRepository(t)
	repoB := filepath.Join(t.TempDir(), "second-checkout")
	runGitTest(t, "", "clone", remote, repoB)
	const itemID = "hab-50"
	oldTrackerFor := trackerFor
	trackerFor = func(string) Tracker {
		return trackerStub{items: []Item{{
			ID: itemID, Title: "change", UpdatedAt: "item-rev", Tags: []string{"forest:ready"},
		}}}
	}
	defer func() { trackerFor = oldTrackerFor }()

	cfg := defaultConfig()
	cfg.Repo = admissionTestRepo
	cfg.Flows.Builder.RequireLabels = []string{"forest:ready"}
	cfg.Flows.Builder.ExcludeLabels = nil
	items, err := builderFlow{}.Select(cfg, repoA)
	if err != nil || len(items) != 1 {
		t.Fatalf("builder subjects = (%#v, %v), want one item", items, err)
	}

	branchName := BranchPrefix + encodeBranchID(itemID) + "-change"
	runGitTest(t, repoA, "checkout", "-q", "-b", branchName)
	if err := os.WriteFile(filepath.Join(repoA, "change.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repoA, "add", "change.txt")
	runGitTest(t, repoA, "commit", "-qm", "change")
	runGitTest(t, repoA, "push", "-q", "-u", "origin", branchName)
	branches, err := verifierFlow{}.Select(cfg, repoA)
	if err != nil || len(branches) != 1 {
		t.Fatalf("verifier subjects = (%#v, %v), want one branch", branches, err)
	}
	if canonicalAdmissionKey(items[0]) != canonicalAdmissionKey(branches[0]) {
		t.Fatalf("admission keys differ: item=%q branch=%q", canonicalAdmissionKey(items[0]), canonicalAdmissionKey(branches[0]))
	}

	subjectJSON, err := json.Marshal(items[0])
	if err != nil {
		t.Fatal(err)
	}
	ready, childReady, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	childRelease, release, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer release.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestActOnSubjectAdmissionHelper$")
	cmd.Env = append(os.Environ(),
		"FOREST_ACT_ADMISSION_HELPER="+repoA,
		"FOREST_ACT_SUBJECT="+string(subjectJSON),
	)
	cmd.ExtraFiles = []*os.File{childReady, childRelease}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	helperDone, markWaited := startTestProcess(t, cmd)
	_ = childReady.Close()
	_ = childRelease.Close()

	started := make(chan error, 1)
	go func() {
		var one [1]byte
		_, err := io.ReadFull(ready, one[:])
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("holder readiness: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("holder did not enter Act: %s", stderr.String())
	}

	var contenderActed bytes.Buffer
	code := actOnSubject(admissionProcessFlow{
		name: "verifier", ready: &contenderActed, release: bytes.NewReader([]byte{1}),
	}, cfg, repoB, branches[0], nil)
	if code != codeBusy {
		t.Fatalf("contender Effect code = %d, want %d", code, codeBusy)
	}
	if contenderActed.Len() != 0 {
		t.Fatal("busy contender entered Act")
	}
	if _, err := release.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-helperDone:
		markWaited()
		if err != nil {
			t.Fatalf("holder process: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("holder did not finish: %s", stderr.String())
	}

	holderRows, _, err := loadLedger(ledgerPath(repoA))
	if err != nil {
		t.Fatal(err)
	}
	contenderRows, _, err := loadLedger(ledgerPath(repoB))
	if err != nil {
		t.Fatal(err)
	}
	if len(holderRows) != 1 || holderRows[0].Subject != items[0].Key || len(contenderRows) != 0 {
		t.Fatalf("Ledger rows = holder %#v, contender %#v; want one holder outcome for %q", holderRows, contenderRows, items[0].Key)
	}
}

func TestRunFlowPassContinuesAfterBusySubject(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	busy := Subject{Key: "item-21", Kind: "item", ID: "21", Revision: "r1"}
	available := Subject{Key: "item-22", Kind: "item", ID: "22", Revision: "r2"}
	release, err := claimAdmission(repo, admissionTestRepo, "verifier", busy)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	var acted []string
	code, key := runFlowPass(selectionFlow{
		subjects: []Subject{busy, available}, acted: &acted,
	}, Config{Repo: admissionTestRepo}, repo, nil)
	if code != 0 || key != available.Key {
		t.Fatalf("runFlowPass = (%d, %q), want success for %q", code, key, available.Key)
	}
	if len(acted) != 1 || acted[0] != available.Key {
		t.Fatalf("Act subjects = %v, want only %q", acted, available.Key)
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil || len(rows) != 1 || rows[0].Subject != available.Key {
		t.Fatalf("Ledger = (%#v, %v), want one %q outcome", rows, err, available.Key)
	}
}
