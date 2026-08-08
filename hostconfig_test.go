package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hostConfigRepo is a throwaway repository whose forest.yaml is committed, the
// shape the factory's own checkout has when the daemon runs. Tests drive the
// host-config gate against working-tree states built from it.
func hostConfigRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGitTest(t, repo, "init", "-b", "master")
	runGitTest(t, repo, "config", "user.email", "test@example.com")
	runGitTest(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, hostConfigName),
		[]byte("repo: o/r\nchecks:\n  - name: x\n    run: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", hostConfigName)
	runGitTest(t, repo, "commit", "-qm", "init")
	return repo
}

// rewriteHostConfig writes body over the worktree's forest.yaml, the way an
// agent that reached outside its worktree would, leaving it uncommitted.
func rewriteHostConfig(t *testing.T, repo, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, hostConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestVerifyHostConfigAllowsCommittedConfig pins the accepted case of the
// oracle: when the host forest.yaml equals the committed revision, every flow
// may act.
func TestVerifyHostConfigAllowsCommittedConfig(t *testing.T) {
	if err := verifyHostConfig(hostConfigRepo(t)); err != nil {
		t.Fatalf("verifyHostConfig rejected a clean host config: %v", err)
	}
}

// TestVerifyHostConfigRejectsOutOfBandWrite pins the oracle: a run that edited
// the host forest.yaml outside its worktree makes the next pass refuse and name
// the file. The refusal must name the file so an operator knows what to inspect.
func TestVerifyHostConfigRejectsOutOfBandWrite(t *testing.T) {
	repo := hostConfigRepo(t)
	rewriteHostConfig(t, repo, "repo: o/r\nchecks:\n  - name: pwn\n    run: touch $HOME/pwned\n")
	err := verifyHostConfig(repo)
	if err == nil {
		t.Fatal("verifyHostConfig accepted a host forest.yaml modified outside the worktree")
	}
	if !strings.Contains(err.Error(), hostConfigName) {
		t.Fatalf("refusal did not name the file %s: %v", hostConfigName, err)
	}
}

// TestVerifyHostConfigAllowsCommittedChange pins that a committed operator edit
// to the host config is legitimate work: HEAD moves with the commit, the diff
// returns to empty, and the factory acts again. The control is the committed
// channel (review + merge), not an uncommitted working-tree write.
func TestVerifyHostConfigAllowsCommittedChange(t *testing.T) {
	repo := hostConfigRepo(t)
	rewriteHostConfig(t, repo, "repo: o/r\nchecks:\n  - name: y\n    run: echo hi\n")
	runGitTest(t, repo, "add", hostConfigName)
	runGitTest(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-qm", "config change")
	if err := verifyHostConfig(repo); err != nil {
		t.Fatalf("verifyHostConfig rejected a committed operator config change: %v", err)
	}
}

// TestVerifyHostConfigRejectsStagedWrite pins that a host-config change staged
// but not committed is an out-of-band write just the same: `git status
// --porcelain` reports it whether it is staged or not, so the gate closes the
// staged-but-uncommitted window that a `git diff` between HEAD and the worktree
// alone would leave open.
func TestVerifyHostConfigRejectsStagedWrite(t *testing.T) {
	repo := hostConfigRepo(t)
	rewriteHostConfig(t, repo, "repo: o/r\nchecks:\n  - name: pwn\n    run: touch /tmp/pwned\n")
	runGitTest(t, repo, "add", hostConfigName)
	if err := verifyHostConfig(repo); err == nil {
		t.Fatal("verifyHostConfig accepted a staged-but-uncommitted host config write")
	}
}

type hostConfigProbeFlow struct {
	acted chan struct{}
}

func (hostConfigProbeFlow) Name() string                  { return "probe" }
func (hostConfigProbeFlow) Interval(Config) time.Duration { return 5 * time.Millisecond }
func (hostConfigProbeFlow) Enabled(Config) bool           { return true }
func (hostConfigProbeFlow) Select(Config, string) ([]Subject, error) {
	return []Subject{{Key: "probe", Revision: "revision"}}, nil
}
func (f hostConfigProbeFlow) Act(Config, string, Subject, string) (Outcome, error) {
	select {
	case f.acted <- struct{}{}:
	default:
	}
	return Outcome{Status: "done"}, nil
}

func TestRunFlowLoopRefusesDirtyHostConfig(t *testing.T) {
	repo := hostConfigRepo(t)
	rewriteHostConfig(t, repo, "repo: o/r\nchecks:\n  - name: pwn\n    run: touch /tmp/pwned\n")
	flow := hostConfigProbeFlow{acted: make(chan struct{}, 1)}
	var drain int32
	done := make(chan struct{})
	go func() {
		runFlowLoop(flow, defaultConfig(), repo, &drain, nil)
		close(done)
	}()
	select {
	case <-flow.acted:
		t.Fatal("runFlowLoop acted with a dirty host config")
	case <-time.After(100 * time.Millisecond):
	}
	atomic.StoreInt32(&drain, 1)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runFlowLoop did not drain")
	}
}
