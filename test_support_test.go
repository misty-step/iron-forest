package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeTestDeclaration(t *testing.T, root, agent string) {
	dir := filepath.Join(root, "agents", agent)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte("---\nmodel: local\n---\nsystem\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "task.md"), []byte("task\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runGit(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return output
}

func runGitDir(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %v: %v\n%s", dir, args, err, output)
	}
	return output
}

func configGit(t *testing.T, dir, name, email string) {
	t.Helper()
	runGitDir(t, dir, "config", "user.name", name)
	runGitDir(t, dir, "config", "user.email", email)
}

func addNote(t *testing.T, root, ref, sha, payload, name, email string) {
	t.Helper()
	cmd := exec.Command("git", "-C", root, "-c", "user.name="+name, "-c", "user.email="+email, "notes", "--ref="+ref, "add", "-m", payload, sha)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add note: %v\n%s", err, output)
	}
}

func pollReviewNote(sha string) string {
	return pollReviewNoteBranch(sha, "forest/4-work")
}

func pollReviewNoteBranch(sha, branch string) string {
	return `{"schema":"forest.review-request.v1","issue":4,"branch":"` + branch + `","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`
}

func processHeartbeatFixture(t *testing.T) (string, string) {
	t.Helper()
	state := t.TempDir()
	heartbeat := filepath.Join(state, "heartbeat")
	childPID := filepath.Join(state, "child-pid")
	t.Setenv("HEARTBEAT", heartbeat)
	t.Setenv("CHILD_PID", childPID)
	t.Cleanup(func() {
		body, err := os.ReadFile(childPID)
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
		if err != nil || pid <= 1 {
			return
		}
		process, err := os.FindProcess(pid)
		if err == nil {
			_ = process.Kill()
		}
	})
	return state, heartbeat
}

func assertProcessQuiescent(t *testing.T, heartbeat, subject, stoppedBy string) {
	t.Helper()
	before, err := os.ReadFile(heartbeat)
	if err != nil || len(before) == 0 {
		t.Fatalf("%s produced no heartbeat: %d bytes, %v", subject, len(before), err)
	}
	time.Sleep(300 * time.Millisecond)
	after, err := os.ReadFile(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("%s survived %s: heartbeat grew from %d to %d bytes", subject, stoppedBy, len(before), len(after))
	}
}

func testClone(t *testing.T) (string, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "clone")
	origin := filepath.Join(t.TempDir(), "origin.git")
	runGit(t, "init", "--bare", origin)
	runGit(t, "clone", origin, root)
	configGit(t, root, "Builder", "builder@forest.invalid")
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("initial\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := []byte(`repo: owner/name
agents:
  builder: {poll: "forest poll builder", interval: 1, timeout: 1}
checks:
  - {name: test, run: "go test ./..."}
`)
	if err := os.WriteFile(filepath.Join(root, "forest.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{
		filepath.Join("agents", "_shared", "skills", "shared", "SKILL.md"),
		filepath.Join("agents", "builder", "skills", "builder", "SKILL.md"),
		filepath.Join("agents", "verifier", "skills", "verifier", "SKILL.md"),
		filepath.Join("agents", "fixer", "skills", "fixer", "SKILL.md"),
	} {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("# Fixture\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitDir(t, root, "add", "file", "forest.yaml", "agents")
	runGitDir(t, root, "commit", "-m", "initial")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")
	return root, origin
}

func testGitTransportStopsDescendants(t *testing.T, name, wantOutput string, run func(context.Context, string) ([]byte, error)) {
	t.Helper()
	tests := []struct {
		name       string
		leaderTail string
		timeout    time.Duration
		wantErr    error
	}{
		{name: "leader success", leaderTail: "exit 0\n", timeout: 3 * time.Second},
		{name: "cancellation", leaderTail: "trap '' TERM\nwhile :; do /bin/sleep 1; done\n", timeout: 100 * time.Millisecond, wantErr: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, toolDir := t.TempDir(), t.TempDir()
			_, heartbeat := processHeartbeatFixture(t)
			script := `#!/bin/sh
set -eu
(
	trap '' HUP TERM
	while :; do
		printf x >> "$HEARTBEAT"
		/bin/sleep 0.02
	done
) &
child=$!
printf '%s\n' "$child" > "$CHILD_PID"
while [ ! -s "$HEARTBEAT" ]; do /bin/sleep 0.01; done
printf '%s\n' ` + wantOutput + "\n" + test.leaderTail
			if err := os.WriteFile(filepath.Join(toolDir, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", toolDir)
			ctx, cancel := context.WithTimeout(context.Background(), test.timeout)
			started := time.Now()
			output, err := run(ctx, root)
			elapsed := time.Since(started)
			cancel()
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("%s transport error=%v want %v", name, err, test.wantErr)
			}
			if string(output) != wantOutput+"\n" {
				t.Fatalf("%s transport output=%q", name, output)
			}
			if elapsed >= 3*time.Second {
				t.Fatalf("%s transport took %s", name, elapsed)
			}
			assertProcessQuiescent(t, heartbeat, name+" transport descendant", test.name)
		})
	}
}

func quoteShellPath(path string) string {
	return "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}
