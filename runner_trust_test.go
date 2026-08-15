package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Runner's trust boundary: which executables and paths a Run may use.

func TestTrustedPathExcludesManagedCheckout(t *testing.T) {
	root := t.TempDir()
	safe := t.TempDir()
	t.Setenv("PATH", root+string(os.PathListSeparator)+safe)
	got, err := trustedPath(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != safe {
		t.Fatalf("trusted PATH=%q, want %q", got, safe)
	}
	shadow := filepath.Join(root, "omp")
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := trustedExecutable(root, shadow); err == nil {
		t.Fatal("repository executable accepted")
	}
}

// A version manager dispatches on the name it was invoked as, so the caller's own
// path must be executed even when it is a symlink. Executing the resolved target
// would run the manager instead of the tool. The trust decision still follows the
// symlink, so a link into the repository is still refused.
func TestTrustedExecutableRunsTheCallersPathSoShimsDispatch(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	// A shim that reports the name it was invoked as, like a version manager.
	real := filepath.Join(outside, "manager")
	script := "#!/bin/sh\nprintf '%s\\n' \"$(basename \"$0\")\"\n"
	if err := os.WriteFile(real, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(outside, "pi")
	if err := os.Symlink(real, shim); err != nil {
		t.Fatal(err)
	}

	resolved, err := trustedExecutable(root, shim)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != shim {
		t.Fatalf("resolved=%q, want the caller's path %q so the shim dispatches", resolved, shim)
	}
	output, err := exec.Command(resolved).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(output)) != "pi" {
		t.Fatalf("shim was invoked as %q, want pi", strings.TrimSpace(string(output)))
	}

	// Trust still follows the link: a shim whose target is in the repository is
	// refused even though the link itself sits outside.
	inside := filepath.Join(root, "planted")
	if err := os.WriteFile(inside, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(outside, "planted-link")
	if err := os.Symlink(inside, planted); err != nil {
		t.Fatal(err)
	}
	if _, err := trustedExecutable(root, planted); err == nil {
		t.Fatal("accepted a shim whose target is inside the repository")
	}
}

// A harness that cannot be executed has produced no usage, so demanding usage
// would report "no usage" as the cause and bury the real one.
func TestRunReportsOnlyTheHarnessFailureWhenItCannotStart(t *testing.T) {
	root, _ := testClone(t)
	runner := NewRunner(root)
	runner.PiPath = filepath.Join(t.TempDir(), "absent-harness")

	declaration := Declaration{Name: "builder", Model: "local", SystemPrompt: "system", TaskPrompt: "Reply"}
	record, err := runner.Run(context.Background(), declaration)
	if err == nil {
		t.Fatal("a missing harness must fail the Run")
	}
	if strings.Contains(err.Error(), "no usage") {
		t.Fatalf("error buries the cause behind a usage complaint: %v", err)
	}
	if record.Exit != harnessUnavailableExit {
		t.Fatalf("exit=%d, want %d (err=%v)", record.Exit, harnessUnavailableExit, err)
	}
	if record.TokensIn != 0 || record.TokensOut != 0 {
		t.Fatalf("record=%+v, want zero tokens: nothing ran", record)
	}
}

func TestToolEntrypointsRejectRepositoryExecutablesThroughSymlinkedPath(t *testing.T) {
	root := t.TempDir()
	repositoryBin := filepath.Join(root, "bin")
	if err := os.Mkdir(repositoryBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"git", "gh", "pi", "rm"} {
		if err := os.WriteFile(filepath.Join(repositoryBin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	pathEntry := filepath.Join(outside, "bin")
	if err := os.Symlink(repositoryBin, pathEntry); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", pathEntry)
	runner := NewRunner(root)
	poller := NewPoller(root, "owner/name")
	tests := []struct {
		name string
		run  func() error
	}{
		{name: "git", run: func() error {
			_, err := runner.git(context.Background(), root, "--version")
			return err
		}},
		{name: "poller git", run: func() error {
			_, err := poller.run(context.Background(), "git", "--version")
			return err
		}},
		{name: "gh", run: func() error {
			_, err := poller.gh(context.Background(), "--version")
			return err
		}},
		{name: "pi", run: func() error {
			_, err := runner.piExecutable()
			return err
		}},
		{name: "rm", run: func() error {
			return runner.removeFilesystem(context.Background(), filepath.Join(root, "worktrees", "missing"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatalf("accepted repository %s executable", test.name)
			}
		})
	}
}
