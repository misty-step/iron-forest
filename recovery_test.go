package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInterruptedStartupPreservesNativeGitSource(t *testing.T) {
	root, _ := testClone(t)
	id := newRunID("builder", time.Now())
	path := forestPath(root, "worktrees", id)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "worktree", "add", "--detach", path, "HEAD")
	committed := []byte("unpublished committed bytes\n")
	if err := os.WriteFile(filepath.Join(path, "committed.bin"), committed, 0o600); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, path, "add", "committed.bin")
	runGitDir(t, path, "-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "commit", "-m", "unpublished")
	base := strings.TrimSpace(string(runGitDir(t, path, "rev-parse", "HEAD")))
	runGitDir(t, path, "rm", "committed.bin")
	staged := bytes.Repeat([]byte{0, 255, 42, 10}, 512*1024)
	working, ignored := []byte("working\x00\xfe"), []byte("ignored\x00source")
	if err := os.WriteFile(filepath.Join(path, "large.bin"), staged, 0o600); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, path, "add", "large.bin")
	if err := os.WriteFile(filepath.Join(path, "large.bin"), working, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("ignored.bin\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "ignored.bin"), ignored, 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(path, "link")); err != nil {
		t.Fatal(err)
	}
	if err := writeLiveRun(liveRunPath(root, "builder"), liveRunRecord{RunID: id, Agent: "builder"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := recoverInterruptedRuns(root); err != nil {
			t.Fatal(err)
		}
		if err := cleanupReservedResidue(root, NewRunner(root)); err != nil {
			t.Fatal(err)
		}
	}
	record, found, err := FindRun(root, id)
	if err != nil || !found || record.Outcome != runOutcomeInterrupted || record.Recovery == nil || record.Recovery.BaseRevision != base || record.Recovery.Path != filepath.Join(workspaceName, "worktrees", id) {
		t.Fatalf("lost interrupted source evidence: %#v %v", record, err)
	}
	rows, err := readLedger(root, -1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("startup duplicated Run: %v %v", rows, err)
	}
	runGitDir(t, root, "reflog", "expire", "--expire=now", "--all")
	runGitDir(t, root, "gc", "--prune=now")
	if got := runGitDir(t, path, "show", "HEAD:committed.bin"); !bytes.Equal(got, committed) {
		t.Fatalf("unpublished base lost: %q", got)
	}
	if got := runGitDir(t, path, "show", ":large.bin"); !bytes.Equal(got, staged) {
		t.Fatal("large staged binary changed")
	}
	for name, want := range map[string][]byte{"large.bin": working, "ignored.bin": ignored, "link": []byte("outside")} {
		got, err := os.ReadFile(filepath.Join(path, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("native source %s changed: %q %v", name, got, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(path, "link")); err != nil || target != outside {
		t.Fatalf("link changed: %s %v", target, err)
	}
	if _, err := os.Stat(filepath.Join(path, "committed.bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged deletion lost: %v", err)
	}
}

func TestRunnerCancellationPreservesSourceAndAllowsNextRun(t *testing.T) {
	root, _ := testClone(t)
	state := t.TempDir()
	ready := filepath.Join(state, "ready")
	t.Setenv("RECOVERY_READY", ready)
	pi := filepath.Join(state, "pi")
	script := "#!/bin/sh\nset -eu\nprintf 'tracked\\000bytes' > source.bin\ngit add source.bin\nprintf 'draft\\000bytes' > draft.bin\nprintf ready > \"$RECOVERY_READY\"\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(pi, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(root)
	runner.PiPath = pi
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan RunRecord, 1)
	go func() {
		record, _ := runner.Run(ctx, Declaration{Name: "builder", Model: "local", TaskPrompt: "fixture"})
		done <- record
	}()
	waitForCLIFile(t, ready)
	live, err := readLiveRuns(root)
	if err != nil || len(live) != 1 {
		t.Fatalf("live Run unavailable: %v %v", live, err)
	}
	code, stdout, stderr := captureCLIOutput(t, func() int { return runSurfaceCommand([]string{"run", "cancel", live[0].RunID, "--root", root}) })
	if code != exitOK {
		t.Fatalf("cancel failed: %d %s %s", code, stdout, stderr)
	}
	select {
	case record := <-done:
		if record.Outcome != runOutcomeCancelled || record.Exit != runCancelledExit || record.Recovery == nil {
			t.Fatalf("lost cancellation custody: %#v", record)
		}
		path := filepath.Join(root, record.Recovery.Path)
		for name, want := range map[string][]byte{"source.bin": []byte("tracked\x00bytes"), "draft.bin": []byte("draft\x00bytes")} {
			got, err := os.ReadFile(filepath.Join(path, name))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("cancelled source %s changed: %q %v", name, got, err)
			}
		}
		if err := cleanupReservedResidue(root, runner); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pi, []byte("#!/bin/sh\nprintf '%s\\n' '{\"usage\":{\"input\":0}}'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		next, err := runner.Run(context.Background(), Declaration{Name: "builder", Model: "local", TaskPrompt: "independent"})
		if err != nil || next.Outcome != runOutcomeCompleted || next.Recovery != nil {
			t.Fatalf("retained source blocked independent Run: %#v %v", next, err)
		}
		if _, err := os.Stat(forestPath(root, "worktrees", next.RunID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("successful worktree survived: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("next Run removed retained source: %v", err)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("cancelled Run did not finalize")
	}
}

func TestRecoveryAppendRetryPreservesFinalizedExecution(t *testing.T) {
	for _, outcome := range []string{runOutcomeCompleted, runOutcomeProviderFailed} {
		t.Run(outcome, func(t *testing.T) {
			root := t.TempDir()
			rawExit := 0
			record := RunRecord{RunID: "1-builder", Agent: "builder", Outcome: outcome, ProcessExit: &rawExit}
			if outcome == runOutcomeProviderFailed {
				record.Exit = 1
			}
			live := liveRecord(record)
			live.Finalized = true
			if err := writeLiveRun(liveRunPath(root, record.Agent), live); err != nil {
				t.Fatal(err)
			}
			blocker := forestPath(root, ".runs.jsonl-blocked")
			if err := os.Mkdir(blocker, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(blocker, "child"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := recoverInterruptedRuns(root); err == nil {
				t.Fatal("blocked Ledger append succeeded")
			}
			if err := os.RemoveAll(blocker); err != nil {
				t.Fatal(err)
			}
			if err := recoverInterruptedRuns(root); err != nil {
				t.Fatal(err)
			}
			retained, found, err := FindRun(root, record.RunID)
			if err != nil || !found || retained.Outcome != outcome || retained.Exit != record.Exit || retained.ProcessExit == nil || *retained.ProcessExit != rawExit {
				t.Fatalf("retry lost finalized execution: %#v %v", retained, err)
			}
		})
	}
}
