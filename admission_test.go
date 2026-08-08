package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const admissionTestRepo = "test/admission"

func putTestAdmissionClaim(t *testing.T, repo, key, host string) {
	t.Helper()
	body, err := json.Marshal(admissionClaim{
		Flow: "builder", Host: host, Revision: "foreign",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlobRef(repo, admissionRef(key), string(body), ""); err != nil {
		t.Fatal(err)
	}
}

func captureTestStderr(t *testing.T, fn func()) string {
	t.Helper()
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = writeEnd
	t.Cleanup(func() { os.Stderr = oldStderr })
	fn()
	_ = writeEnd.Close()
	os.Stderr = oldStderr
	output, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatal(err)
	}
	_ = readEnd.Close()
	return string(output)
}

func TestCanonicalAdmissionIdentity(t *testing.T) {
	item := Subject{Key: "item-hab-01", Kind: "item", ID: "hab-01"}
	branch := Subject{Key: "branch-forest/hab-01-change", Kind: "branch", ID: "hab-01"}
	retirement := Subject{Key: "branch-forest/hab-01-change", Kind: "retirement", ID: "hab-01"}
	if got, want := canonicalAdmissionKey(item), "item-hab-01"; got != want {
		t.Fatalf("item admission key = %q, want %q", got, want)
	}
	if got, want := canonicalAdmissionKey(branch), "item-hab-01"; got != want {
		t.Fatalf("branch admission key = %q, want %q", got, want)
	}
	if got, want := canonicalAdmissionKey(retirement), "item-hab-01"; got != want {
		t.Fatalf("retirement admission key = %q, want %q", got, want)
	}
	if got, want := canonicalAdmissionKey(Subject{Key: managerSubject, Kind: "manager"}), managerSubject; got != want {
		t.Fatalf("singleton admission key = %q, want %q", got, want)
	}
}

func TestAdmissionOpaqueIdentityUsesValidRef(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	s := Subject{Key: "item-opaque", Kind: "item", ID: "hab /?*[~^:"}
	release, err := claimAdmission(repo, admissionTestRepo, "builder", s)
	if err != nil {
		t.Fatalf("claim opaque Subject: %v", err)
	}
	defer release()
	if _, err := gitOut(repo, "check-ref-format", admissionRef(canonicalAdmissionKey(s))); err != nil {
		t.Fatalf("opaque admission ref: %v", err)
	}
}

func TestHostIdentityFailsClosed(t *testing.T) {
	for _, input := range []string{"", "uninitialized", strings.Repeat("0", 32), "1234"} {
		if got := hostIDFromMachineID(input, os.Getuid()); got != "" {
			t.Errorf("hostIDFromMachineID(%q) = %q, want empty", input, got)
		}
	}
	valid := "0123456789abcdef0123456789abcdef\n"
	owner := hostIDFromMachineID(valid, os.Getuid())
	if owner == "" {
		t.Fatal("valid machine and owner identity produced no Host identity")
	}
	if other := hostIDFromMachineID(valid, os.Getuid()+1); other == owner {
		t.Fatal("different Unix owners share a Host identity")
	}
	body, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := localHostID(), hostIDFromMachineID(string(body), os.Getuid()); got != want || got == "" {
		t.Fatalf("localHostID = %q, want parsed machine and owner identity %q", got, want)
	}
}

func TestAdmissionLockPathIsPerUserAndStable(t *testing.T) {
	root := filepath.Join("/tmp", fmt.Sprintf("iron-forest-%d", os.Getuid()), "admission")
	got := admissionLockPath("Owner/Repo", "item-1")
	if filepath.Dir(got) != root {
		t.Fatalf("admission lock directory = %q, want %q", filepath.Dir(got), root)
	}
	if same := admissionLockPath("owner/repo", "item-1"); same != got {
		t.Fatalf("case alias lock = %q, want %q", same, got)
	}
	if other := admissionLockPath("owner/other", "item-1"); other == got {
		t.Fatal("different repositories share an admission lock")
	}
	if other := admissionLockPath("owner/repo", "item-2"); other == got {
		t.Fatal("different Subjects share an admission lock")
	}
}

func TestLoadConfigRejectsRepositoryAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forest.yaml")
	for _, repo := range []string{"github.com/owner/repo", "owner/repo/", "owner//repo", "owner", "owner/re po", "owner/ repo", "owner/repo.git"} {
		body := fmt.Sprintf("repo: %s\nchecks:\n  - name: test\n    run: \"true\"\n", repo)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "canonical owner/name") {
			t.Fatalf("loadConfig repository %q = %v, want canonical owner/name error", repo, err)
		}
	}
}
func TestLoadConfigRejectsHostFastForward(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forest.yaml")
	body := "repo: owner/repo\nchecks:\n  - name: test\n    run: \"true\"\n" +
		"flows:\n  verifier:\n    merge: ff\n" +
		"projection:\n  enabled: true\n  merge_via_host: true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "supports only squash") {
		t.Fatalf("loadConfig Host fast-forward = %v, want refusal", err)
	}
}

func TestLoadConfigRejectsManagerLabelDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forest.yaml")
	for _, labels := range []string{"[team:ready]", "[forest:ready, extra]"} {
		body := "repo: owner/repo\nchecks:\n  - name: test\n    run: \"true\"\n" +
			"flows:\n  builder:\n    require_labels: " + labels + "\n" +
			"  manager:\n    enabled: true\n"
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "enabled Manager requires Builder require_labels") {
			t.Fatalf("loadConfig Manager labels %s = %v, want exact-label refusal", labels, err)
		}
	}
	body := "repo: owner/repo\nchecks:\n  - name: test\n    run: \"true\"\n" +
		"flows:\n  builder:\n    require_labels: [forest:ready]\n" +
		"    exclude_labels: [forest:ready]\n  manager:\n    enabled: true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "remain eligible") {
		t.Fatalf("loadConfig Builder exclusion of Manager output = %v, want refusal", err)
	}
}

func TestLoadConfigRejectsTrailingDocumentAndBlankCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forest.yaml")
	tests := map[string]string{
		"trailing document": "repo: owner/repo\nchecks:\n  - name: test\n    run: \"true\"\n---\nretired: true\n",
		"blank name":        "repo: owner/repo\nchecks:\n  - name: \"  \"\n    run: \"true\"\n",
		"blank run":         "repo: owner/repo\nchecks:\n  - name: test\n    run: \"  \"\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfig(path); err == nil {
				t.Fatal("loadConfig accepted malformed configuration")
			}
		})
	}
}

func TestLoadConfigRejectsHostMergeWithoutProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forest.yaml")
	body := "repo: owner/repo\nchecks:\n  - name: test\n    run: \"true\"\n" +
		"projection:\n  enabled: false\n  merge_via_host: true\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil || !strings.Contains(err.Error(), "requires projection.enabled") {
		t.Fatalf("loadConfig Host merge without Projection = %v, want refusal", err)
	}
}

func TestLoadConfigAcceptsCanonicalRepository(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forest.yaml")
	if err := os.WriteFile(path, []byte("repo: owner/repo\nchecks:\n  - name: test\n    run: \"true\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, err := loadConfig(path); err != nil || cfg.Repo != "owner/repo" {
		t.Fatalf("loadConfig canonical repository = (%q, %v), want owner/repo", cfg.Repo, err)
	}
}

func TestAdmissionSameCheckoutBusy(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	s := Subject{Key: "item-41", Kind: "item", ID: "41"}

	release, err := claimAdmission(repo, admissionTestRepo, "builder", s)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	firstSHA, firstBody, err := getBlobRef(repo, admissionRef(canonicalAdmissionKey(s)))
	if err != nil || firstSHA == "" {
		t.Fatalf("first claim Revision = %q, %v", firstSHA, err)
	}
	var first admissionClaim
	if err := json.Unmarshal([]byte(firstBody), &first); err != nil {
		t.Fatal(err)
	}
	revision, err := hex.DecodeString(first.Revision)
	if err != nil || len(revision) != 16 {
		t.Fatalf("claim Revision = %q, %v; want 16 random bytes", first.Revision, err)
	}
	if _, err := claimAdmission(repo, admissionTestRepo, "builder", s); !errors.Is(err, errAdmissionHeld) {
		t.Fatalf("second claim = %v, want admission refusal", err)
	}
	release()

	release, err = claimAdmission(repo, admissionTestRepo, "builder", s)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	secondSHA, _, err := getBlobRef(repo, admissionRef(canonicalAdmissionKey(s)))
	if err != nil || secondSHA == "" || secondSHA == firstSHA {
		t.Fatalf("second claim Revision = %q, %v; want a new SHA after %q", secondSHA, err, firstSHA)
	}
	release()
}

func TestAdmissionPrepublicationHelper(t *testing.T) {
	repo := os.Getenv("FOREST_ADMISSION_PREPUBLISH_HELPER")
	if repo == "" {
		return
	}
	release, err := claimAdmission(repo, admissionTestRepo, "builder", Subject{
		Key: "item-16", Kind: "item", ID: "16",
	})
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestAdmissionLockRefusesBeforeClaimPublication(t *testing.T) {
	repoA, repoB := newAdmissionRepositories(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	if err := os.WriteFile(wrapper, []byte(`#!/bin/sh
case " $* " in
  *" ls-remote "*)
    printf x >&3
    IFS= read -r release <&4
    ;;
esac
exec "$FOREST_REAL_GIT" "$@"
`), 0o755); err != nil {
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestAdmissionPrepublicationHelper$")
	cmd.Env = append(os.Environ(),
		"FOREST_ADMISSION_PREPUBLISH_HELPER="+repoA,
		"FOREST_REAL_GIT="+realGit,
		"PATH="+wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.ExtraFiles = []*os.File{childReady, childRelease}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	helperDone, markWaited := startTestProcess(t, cmd)
	_ = childReady.Close()
	_ = childRelease.Close()

	reachedRead := make(chan error, 1)
	go func() {
		var one [1]byte
		_, err := io.ReadFull(ready, one[:])
		reachedRead <- err
	}()
	select {
	case err := <-reachedRead:
		if err != nil {
			t.Fatalf("claim helper did not reach prepublication read: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("claim helper did not reach prepublication read: %s", stderr.String())
	}

	s := Subject{Key: "item-16", Kind: "item", ID: "16"}
	contenderRelease, err := claimAdmission(repoB, admissionTestRepo, "verifier", s)
	if err == nil {
		contenderRelease()
		t.Fatal("contender entered before the holder published its claim")
	}
	if !errors.Is(err, errAdmissionHeld) {
		t.Fatalf("prepublication contender = %v, want admission refusal", err)
	}
	if _, err := release.Write([]byte{'\n'}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-helperDone:
		markWaited()
		if err != nil {
			t.Fatalf("claim helper: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("claim helper did not finish: %s", stderr.String())
	}
	if sha, _, err := getBlobRef(repoB, admissionRef(canonicalAdmissionKey(s))); err != nil || sha != "" {
		t.Fatalf("claim after holder release = %q, %v; want absent", sha, err)
	}
}

func TestAdmissionCrashHelper(t *testing.T) {
	repo := os.Getenv("FOREST_ADMISSION_CRASH_HELPER")
	if repo == "" {
		return
	}
	release, err := claimAdmission(repo, admissionTestRepo, "builder", Subject{
		Key: "item-7", Kind: "item", ID: "7",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ready := os.NewFile(3, "ready")
	if ready == nil {
		t.Fatal("missing readiness pipe")
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := ready.Close(); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestAdmissionCrossProcessBusyAndHardKillReclaims(t *testing.T) {
	repoA, repoB := newAdmissionRepositories(t)
	childTmp := t.TempDir()
	ready, signal, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer signal.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAdmissionCrashHelper$")
	cmd.Env = append(os.Environ(),
		"FOREST_ADMISSION_CRASH_HELPER="+repoA,
		"TMPDIR="+childTmp,
	)
	cmd.ExtraFiles = []*os.File{signal}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	helperDone, markWaited := startTestProcess(t, cmd)
	_ = signal.Close()

	started := make(chan error, 1)
	go func() {
		var one [1]byte
		_, err := io.ReadFull(ready, one[:])
		started <- err
	}()
	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("crash helper readiness: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("crash helper did not claim admission: %s", stderr.String())
	}

	s := Subject{Key: "item-7", Kind: "item", ID: "7"}
	if _, err := claimAdmission(repoB, admissionTestRepo, "verifier", s); !errors.Is(err, errAdmissionHeld) {
		t.Fatalf("cross-process claim = %v, want admission refusal", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := <-helperDone; err == nil {
		t.Fatal("crash helper exited without a hard kill")
	}
	markWaited()

	release, err := claimAdmission(repoA, admissionTestRepo, "builder", s)
	if err != nil {
		t.Fatalf("reclaim after hard kill: %v", err)
	}
	release()
	if sha, _, err := getBlobRef(repoA, admissionRef(canonicalAdmissionKey(s))); err != nil || sha != "" {
		t.Fatalf("claim after reclaim release = %q, %v; want absent", sha, err)
	}
}

func TestAdmissionForeignHostRefuses(t *testing.T) {
	repoA, repoB := newAdmissionRepositories(t)
	s := Subject{Key: "item-9", Kind: "item", ID: "9"}
	key := canonicalAdmissionKey(s)
	foreign := hostID + "-foreign"
	putTestAdmissionClaim(t, repoA, key, foreign)
	beforeSHA, beforeBody, err := getBlobRef(repoB, admissionRef(key))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := claimAdmission(repoB, admissionTestRepo, "builder", s); !errors.Is(err, errAdmissionHeld) {
		t.Fatalf("foreign-host claim = %v, want admission refusal", err)
	}
	sha, body, err := getBlobRef(repoB, admissionRef(key))
	if err != nil || sha != beforeSHA || body != beforeBody {
		t.Fatalf("foreign claim after refusal = %q %q, %v; want %q %q", sha, body, err, beforeSHA, beforeBody)
	}
}

func TestAdmissionMissingHostIdentityFailsClosed(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	oldHostID := hostID
	hostID = ""
	defer func() { hostID = oldHostID }()
	if _, err := claimAdmission(repo, admissionTestRepo, "builder", Subject{
		Key: "item-10", Kind: "item", ID: "10",
	}); err == nil || errors.Is(err, errAdmissionHeld) {
		t.Fatalf("missing Host identity = %v, want a non-busy error", err)
	}
}

func TestAdmissionCASRaceHelper(t *testing.T) {
	repo := os.Getenv("FOREST_ADMISSION_CAS_HELPER")
	if repo == "" {
		return
	}
	if _, err := claimAdmission(repo, admissionTestRepo, "builder", Subject{
		Key: "item-14", Kind: "item", ID: "14",
	}); !errors.Is(err, errAdmissionHeld) {
		t.Fatalf("claim race = %v, want admission refusal", err)
	}
}

func TestAdmissionCASRacePreservesRemoteWinner(t *testing.T) {
	repoA, repoB := newAdmissionRepositories(t)
	key := canonicalAdmissionKey(Subject{Key: "item-14", Kind: "item", ID: "14"})
	staleBody, err := json.Marshal(admissionClaim{
		Flow: "builder", Host: hostID, Revision: "stale",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlobRef(repoA, admissionRef(key), string(staleBody), ""); err != nil {
		t.Fatal(err)
	}
	staleSHA := blobSHA(string(staleBody))

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapperDir := t.TempDir()
	wrapper := filepath.Join(wrapperDir, "git")
	if err := os.WriteFile(wrapper, []byte(`#!/bin/sh
case " $* " in
  *" push "*"refs/forest/claim/"*)
    printf x >&3
    IFS= read -r release <&4
    ;;
esac
exec "$FOREST_REAL_GIT" "$@"
`), 0o755); err != nil {
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
	cmd := exec.Command(os.Args[0], "-test.run=^TestAdmissionCASRaceHelper$")
	cmd.Env = append(os.Environ(),
		"FOREST_ADMISSION_CAS_HELPER="+repoA,
		"FOREST_REAL_GIT="+realGit,
		"PATH="+wrapperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.ExtraFiles = []*os.File{childReady, childRelease}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	helperDone, markWaited := startTestProcess(t, cmd)
	_ = childReady.Close()
	_ = childRelease.Close()
	reachedPush := make(chan error, 1)
	go func() {
		var one [1]byte
		_, err := io.ReadFull(ready, one[:])
		reachedPush <- err
	}()
	select {
	case err := <-reachedPush:
		if err != nil {
			t.Fatalf("claim helper did not reach push: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("claim helper did not reach push: %s", stderr.String())
	}

	winnerBody, err := json.Marshal(admissionClaim{
		Flow: "fixer", Host: hostID, Revision: "winner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlobRef(repoB, admissionRef(key), string(winnerBody), staleSHA); err != nil {
		t.Fatalf("install race winner: %v", err)
	}
	if _, err := release.Write([]byte{'\n'}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-helperDone:
		markWaited()
		if err != nil {
			t.Fatalf("claim helper: %v: %s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("claim helper did not finish: %s", stderr.String())
	}
	sha, body, err := getBlobRef(repoA, admissionRef(key))
	if err != nil {
		t.Fatal(err)
	}
	if sha != blobSHA(string(winnerBody)) || body != string(winnerBody) {
		t.Fatalf("claim race winner = %q %q, want %q %q", sha, body, blobSHA(string(winnerBody)), string(winnerBody))
	}
}

func TestAdmissionReleaseProtectsNewerClaim(t *testing.T) {
	repoA, repoB := newAdmissionRepositories(t)
	s := Subject{Key: "item-12", Kind: "item", ID: "12"}
	key := canonicalAdmissionKey(s)
	release, err := claimAdmission(repoA, admissionTestRepo, "builder", s)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	oldSHA, _, err := getBlobRef(repoA, admissionRef(key))
	if err != nil || oldSHA == "" {
		t.Fatalf("read first claim = %q, %v", oldSHA, err)
	}
	var oldClaim admissionClaim
	if _, oldBody, err := getBlobRef(repoA, admissionRef(key)); err != nil {
		t.Fatal(err)
	} else if err := json.Unmarshal([]byte(oldBody), &oldClaim); err != nil || oldClaim.Revision == "" {
		t.Fatalf("acquired claim = (%#v, %v), want a unique Revision", oldClaim, err)
	}
	newBody, err := json.Marshal(admissionClaim{
		Flow: "builder", Host: hostID, Revision: "newer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := putBlobRef(repoB, admissionRef(key), string(newBody), oldSHA); err != nil {
		t.Fatalf("replace claim: %v", err)
	}

	if output := captureTestStderr(t, release); output != "" {
		t.Fatalf("stale release reported ownership loss as failure: %q", output)
	}
	sha, body, err := getBlobRef(repoA, admissionRef(key))
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" || body != string(newBody) {
		t.Fatalf("newer claim after stale release = %q %q, want %q %q", sha, body, blobSHA(string(newBody)), string(newBody))
	}
}

func TestAdmissionClaimReportsUnchangedRefFailure(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	s := Subject{Key: "item-15", Kind: "item", ID: "15"}
	key := canonicalAdmissionKey(s)
	putTestAdmissionClaim(t, repo, key, hostID)
	beforeSHA, beforeBody, err := getBlobRef(repo, admissionRef(key))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := gitOut(repo, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(remote, "hooks", "update")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'cannot lock ref: deliberate rejection' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := claimAdmission(repo, admissionTestRepo, "builder", s); err == nil || errors.Is(err, errAdmissionHeld) {
		t.Fatalf("unchanged-ref claim error = %v, want a non-busy failure", err)
	}
	sha, body, err := getBlobRef(repo, admissionRef(key))
	if err != nil || sha != beforeSHA || body != beforeBody {
		t.Fatalf("claim after failed write = %q %q, %v; want %q %q", sha, body, err, beforeSHA, beforeBody)
	}
}

func TestAdmissionReleaseReportsDeleteFailure(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	s := Subject{Key: "item-13", Kind: "item", ID: "13"}
	release, err := claimAdmission(repo, admissionTestRepo, "builder", s)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := gitOut(repo, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(remote, "hooks", "update")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'cannot lock ref: deliberate rejection' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	output := captureTestStderr(t, release)
	if !strings.Contains(output, "release admission item-13") {
		t.Fatalf("release error output = %q, want named admission failure", output)
	}
}

func TestActOnSubjectAdmissionBusyWritesNoLedger(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	s := Subject{Key: "item-31", Kind: "item", ID: "31"}
	release, err := claimAdmission(repo, admissionTestRepo, "builder", s)
	if err != nil {
		t.Fatalf("holder claim: %v", err)
	}

	code := actOnSubject(failingFlow{}, Config{Repo: admissionTestRepo}, repo, s, nil)
	if code != codeBusy {
		t.Fatalf("busy action code = %d, want %d", code, codeBusy)
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("busy action wrote %d Ledger rows, want none", len(rows))
	}
	if _, err := os.Stat(ledgerPath(repo)); !os.IsNotExist(err) {
		t.Fatalf("busy action created Ledger at %s: %v", ledgerPath(repo), err)
	}

	release()
	code = actOnSubject(admissionProcessFlow{
		name: "builder", ready: io.Discard, release: bytes.NewReader([]byte{1}),
	}, Config{Repo: admissionTestRepo}, repo, s, nil)
	if code != 0 {
		t.Fatalf("action after release code = %d, want success", code)
	}
	rows, _, err = loadLedger(ledgerPath(repo))
	if err != nil || len(rows) != 1 {
		t.Fatalf("Ledger after release = (%#v, %v), want one outcome", rows, err)
	}
}
