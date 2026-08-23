package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fenced-update shell script is the executable deployment contract, so a
// regression here is caught by asserting the guards the rejected Revision got
// wrong. These are content checks, but they pin the blocking behaviors: the
// updater must stop the service first (rather than polling for an idle window
// while the scheduler can still dispatch), and it must force a fresh audit
// before restart instead of passively waiting for one afterward.
func TestDeployUpdateFenceGuards(t *testing.T) {
	script, err := os.ReadFile("deploy/install-service.sh")
	if err != nil {
		t.Fatal(err)
	}
	content := string(script)

	if strings.Contains(content, "idle_timeout_seconds") {
		t.Fatal("update must not impose a wall-clock idle deadline; the drain has no deadline")
	}
	if strings.Contains(content, "is_idle") {
		t.Fatal("update must not poll for an idle window before stopping; it must stop first")
	}
	if strings.Contains(content, "waiting for an idle window") {
		t.Fatal("update must not wait for an idle window while the scheduler can still dispatch")
	}
	if !strings.Contains(content, `systemctl --user stop "forest@$name"`) {
		t.Fatal("update must request service stop before fetching or rebuilding")
	}
	if !strings.Contains(content, "./forest audit show --rescan --json") {
		t.Fatal("update must force a fresh audit with `forest audit show --rescan` before restart")
	}
	if strings.Contains(content, "audit_timeout_seconds") {
		t.Fatal("update must not passively wait for an audit pass after restart")
	}
	if !strings.Contains(content, `rm -f -- "$prev_binary"`) {
		t.Fatal("rollback must remove forest.prev so the restored pre-change .gitignore keeps the next clean-tree check clean")
	}
}

// TestDeployUpdateDrainsBeforeProceeding runs the real install script against a
// fake systemctl whose stop blocks until the test releases the drain. It proves
// the updater requests the stop before it can observe status or dispatch, and
// that it does not proceed past the stop until the drain completes.
func TestDeployUpdateDrainsBeforeProceeding(t *testing.T) {
	script, err := os.ReadFile("deploy/install-service.sh")
	if err != nil {
		t.Fatal(err)
	}

	temp := t.TempDir()
	home := filepath.Join(temp, "home")
	bin := filepath.Join(temp, "bin")
	factory := filepath.Join(temp, "factory")
	deploy := filepath.Join(factory, "deploy")
	target := filepath.Join(temp, "inst")
	origin := filepath.Join(temp, "origin.git")
	stateDir := filepath.Join(temp, "state")

	for _, dir := range []string{home, bin, deploy, target, origin, stateDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Copy the script under test into a temporary factory so the script's
	// root/factory derivation stays inside the test directory.
	scriptPath := filepath.Join(deploy, "install-service.sh")
	if err := os.WriteFile(scriptPath, script, 0o755); err != nil {
		t.Fatal(err)
	}

	runGitDir(t, factory, "init")
	configGit(t, factory, "Deploy Test", "deploy@forest.invalid")
	runGitDir(t, factory, "add", "deploy/install-service.sh")
	runGitDir(t, factory, "commit", "-m", "installer")

	runGit(t, "init", "--bare", origin)

	runGitDir(t, target, "init")
	configGit(t, target, "Deploy Test", "deploy@forest.invalid")
	if err := os.WriteFile(filepath.Join(target, "forest.yaml"), []byte("repo: owner/name\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "forest"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, target, "add", "forest.yaml", "forest")
	runGitDir(t, target, "commit", "-m", "initial")
	runGitDir(t, target, "remote", "add", "origin", origin)
	runGitDir(t, target, "push", "origin", "HEAD:refs/heads/master")

	envDir := filepath.Join(home, ".config", "iron-forest")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "inst.env"), []byte("OPENROUTER_API_KEY=test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	systemctl := `#!/bin/sh
log() { printf '%s\n' "$1" >> "$FAKE_LOG"; }
verb="${2:-}"
case "$verb" in
  stop)
    log "systemctl-stop"
    while [ ! -e "$FAKE_DRAIN_DONE" ]; do
      sleep 0.02
      if [ -e "$FAKE_ABORT" ]; then exit 1; fi
    done
    printf '%s\n' stopped > "$FAKE_STATE"
    exit 0
    ;;
  is-active)
    log "systemctl-is-active"
    if [ "$(cat "$FAKE_STATE" 2>/dev/null)" = stopped ]; then
      printf '%s\n' inactive
    else
      printf '%s\n' active
    fi
    exit 0
    ;;
  restart)
    log "systemctl-restart"
    printf '%s\n' active > "$FAKE_STATE"
    exit 0
    ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(systemctl), 0o755); err != nil {
		t.Fatal(err)
	}

	mise := `#!/bin/sh
dest=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then dest="$arg"; fi
  prev="$arg"
done
mkdir -p "$(dirname "$dest")"
cat > "$dest" <<'FAKE_FOREST'
#!/bin/sh
printf '%s\n' "forest-$1" >> "$FAKE_LOG"
case "$1" in
  status)
    printf '%s\n' '{"exit":0,"data":{"kernel":{"running_known":true},"triggers":[],"audit":{"last_result":"pass","last_at":"2026-08-22T00:00:00Z"}}}'
    ;;
  selfcheck)
    exit 0
    ;;
  audit)
    printf '%s\n' '{"exit":0,"data":{"last_result":"pass","last_at":"2026-08-22T00:00:00Z"}}'
    ;;
esac
exit 0
FAKE_FOREST
chmod +x "$dest"
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "mise"), []byte(mise), 0o755); err != nil {
		t.Fatal(err)
	}

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitScript := `#!/bin/sh
case "$*" in
  *"ls-remote --symref origin HEAD"*)
    printf 'ref: refs/heads/master\tHEAD\n'
    printf '0000000000000000000000000000000000000000\tHEAD\n'
    exit 0
    ;;
esac
exec "$FAKE_REAL_GIT" "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}

	logPath := filepath.Join(stateDir, "log")
	drainDone := filepath.Join(stateDir, "drain-done")
	abortPath := filepath.Join(stateDir, "abort")
	statePath := filepath.Join(stateDir, "state")

	env := make([]string, 0, len(os.Environ())+6)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "PATH=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+home,
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_LOG="+logPath,
		"FAKE_DRAIN_DONE="+drainDone,
		"FAKE_ABORT="+abortPath,
		"FAKE_STATE="+statePath,
		"FAKE_REAL_GIT="+realGit,
	)

	cmd := exec.Command("bash", scriptPath, "update", "inst")
	cmd.Dir = temp
	cmd.Env = env
	var output lockedBuffer
	cmd.Stdout = &output
	cmd.Stderr = &output

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(drainDone, []byte("done"), 0o644)
		_ = os.WriteFile(abortPath, []byte("abort"), 0o644)
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	waitForLogLine(t, logPath, "systemctl-stop")
	if got := output.String(); strings.Contains(got, "fast-forwarding") {
		t.Fatalf("update proceeded past the drain before it ended:\n%s", got)
	}
	if strings.Contains(output.String(), "waiting for an idle window") {
		t.Fatalf("update polled for an idle window before stopping:\n%s", output.String())
	}

	if err := os.WriteFile(drainDone, []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("update failed: %v\n%s", err, output.String())
	}
	out := output.String()
	if !strings.Contains(out, "updated forest@inst") {
		t.Fatalf("update did not complete successfully:\n%s", out)
	}
	if !strings.Contains(out, "stopping forest@inst (stops new dispatches and drains live Runs)") {
		t.Fatalf("update did not report the stop-first fence:\n%s", out)
	}
	if stopIdx := strings.Index(out, "stopping forest@inst"); stopIdx == -1 {
		t.Fatal("missing stop message")
	} else if fastForwardIdx := strings.Index(out, "fast-forwarding forest@inst"); fastForwardIdx != -1 && fastForwardIdx < stopIdx {
		t.Fatalf("fast-forward happened before the stop request:\n%s", out)
	}
}

func waitForLogLine(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, _ := os.ReadFile(path)
		if strings.Contains(string(body), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %q in %s; log:\n%s", want, path, body)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
