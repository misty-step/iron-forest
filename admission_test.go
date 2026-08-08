package main

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

// newAdmissionRepo builds a bare remote and two checkouts that share it,
// standing in for two checkouts of one repository.
func newAdmissionRepo(t *testing.T) (remote, repoA, repoB string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	repoA = filepath.Join(root, "a")
	repoB = filepath.Join(root, "b")
	runGitTest(t, root, "init", "--bare", "--initial-branch=master", remote)
	for _, repo := range []string{repoA, repoB} {
		runGitTest(t, root, "clone", remote, repo)
	}
	return remote, repoA, repoB
}

// crashHold publishes a claim on repo for key, holds its lock in this test
// process, then drops the flock without removing the file — exactly the durable
// state a participant leaves behind when it is hard-killed mid-run.
func crashHold(t *testing.T, repo, key string) {
	t.Helper()
	lock := admissionLockPath(repo, key)
	f, err := holdLock(lock)
	if err != nil {
		t.Fatalf("hold lock for crash: %v", err)
	}
	payload := `{"key":"` + key + `","flow":"builder","host":"` + hostname + `","lock":"` + lock + `","run_id":"crashed","time":"t"}`
	if err := putBlobRef(repo, admissionRef(key), payload, ""); err != nil {
		t.Fatalf("publish crash claim: %v", err)
	}
	// Drop the flock but leave the file and the ref, as an abrupt process exit
	// would.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("release lock for crash: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close lock for crash: %v", err)
	}
}

func TestAdmissionTwoCheckoutsRaceOneSubject(t *testing.T) {
	_, repoA, repoB := newAdmissionRepo(t)
	s := Subject{Key: "item-41", Kind: "item", ID: "41"}

	relA, err := claimAdmission(repoA, "builder", "run-a", s)
	if err != nil {
		t.Fatalf("first checkout could not claim: %v", err)
	}
	_, err = claimAdmission(repoB, "builder", "run-b", s)
	if !isAdmissionHeld(err) {
		t.Fatalf("second checkout claim = %v, want admission refused", err)
	}

	// Release from the winner; the second checkout can now take it.
	relA()
	relB, err := claimAdmission(repoB, "builder", "run-b", s)
	if err != nil {
		t.Fatalf("second checkout could not claim after release: %v", err)
	}
	relB()

	// The claim is a git fact a fresh clone can observe.
	out, err := gitCommand(repoA, "ls-remote", "origin", "refs/forest/claim/"+s.Key)
	if err != nil {
		t.Fatalf("ls-remote claim: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("claim ref still present after release: %q", out)
	}
}

// TestAdmissionRefusesWhenDoNotSpendBacks the admission call site: when another
// participant holds the Subject, actOnSubject refuses (codeBusy) without
// recording a run, so no agent run is spent.
func TestAdmissionRefusesWithoutSpending(t *testing.T) {
	_, repoA, repoB := newAdmissionRepo(t)
	s := aSubject[0]

	rel, err := claimAdmission(repoA, "builder", "run-a", s)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	defer rel()

	code := actOnSubject(failingFlow{}, Config{}, repoB, s, nil)
	if code != codeBusy {
		t.Fatalf("actOnSubject under a held subject code = %d, want %d", code, codeBusy)
	}
	rows, _, err := loadLedger(ledgerPath(repoB))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refused admission wrote %d ledger rows; want none", len(rows))
	}
}

func TestAdmissionCrashedHolderIsReclaimed(t *testing.T) {
	_, repoA, repoB := newAdmissionRepo(t)
	s := Subject{Key: "item-7", Kind: "item", ID: "7"}

	// The first checkout is hard-killed mid-run: its claim and lock file remain
	// but its flock is gone.
	crashHold(t, repoA, s.Key)

	// A later pass on the second checkout takes the Subject without any operator
	// action and without waiting on a timeout.
	rel, err := claimAdmission(repoB, "builder", "run-after-crash", s)
	if err != nil {
		t.Fatalf("second checkout could not reclaim after crash: %v", err)
	}
	rel()
}

func TestAdmissionForeignHostIsRefused(t *testing.T) {
	_, repoA, repoB := newAdmissionRepo(t)
	s := Subject{Key: "item-9", Kind: "item", ID: "9"}

	old := hostname
	hostname = "some-other-host"
	defer func() { hostname = old }()

	rel, err := claimAdmission(repoA, "builder", "run-a", s)
	if err != nil {
		t.Fatalf("claim on simulated foreign host: %v", err)
	}

	// Restore our own host and try from the second checkout: the foreign holder
	// is not observable here, so admission refuses rather than duplicate work.
	hostname = old
	_, err = claimAdmission(repoB, "builder", "run-b", s)
	if !errors.Is(err, errAdmissionHeld) {
		t.Fatalf("claim beside foreign holder = %v, want admission refused", err)
	}
	rel()
}
