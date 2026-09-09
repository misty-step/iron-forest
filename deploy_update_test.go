package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type deployFixture struct {
	target, factory, factorySHA, priorSHA string
	home, log, release, binary, evidence  string
	command                               *exec.Cmd
	output                                lockedBuffer
}

// The subprocess is the real installer. Git repositories and historical runtime
// evidence are real; only systemd and compilation are replaced at the boundary.
func newDeployFixture(t *testing.T, failure string) *deployFixture {
	t.Helper()
	temp := t.TempDir()
	f := &deployFixture{target: filepath.Join(temp, "inst"), factory: filepath.Join(temp, "factory"), home: filepath.Join(temp, "home"), log: filepath.Join(temp, "events"), release: filepath.Join(temp, "drained"), binary: "#!/bin/sh\n# actual prior binary\nexit 0\n", evidence: "{\"run_id\":\"1-builder\",\"request_id\":\"historical\"}\n"}
	bin := filepath.Join(temp, "bin")
	for _, dir := range []string{f.target, f.factory, f.home, bin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"install-service.sh", "forest@.service", "forest-eval-flywheel@.service", "forest-eval-flywheel@.timer"} {
		data, err := os.ReadFile(filepath.Join("deploy", name))
		if err != nil {
			t.Fatal(err)
		}
		writeTree(t, f.factory, "deploy/"+name, string(data))
	}
	runGitDir(t, f.factory, "init", "--initial-branch=master")
	configGit(t, f.factory, "Deploy Test", "deploy@forest.invalid")
	runGitDir(t, f.factory, "add", "deploy")
	runGitDir(t, f.factory, "commit", "-m", "reviewed installer")
	f.factorySHA = strings.TrimSpace(string(runGitDir(t, f.factory, "rev-parse", "HEAD")))
	origin := filepath.Join(temp, "origin.git")
	runGit(t, "init", "--bare", "--initial-branch=master", origin)
	runGitDir(t, f.target, "init", "--initial-branch=master")
	configGit(t, f.target, "Deploy Test", "deploy@forest.invalid")
	writeTree(t, f.target, "forest.yaml", "repo: owner/name\n")
	writeTree(t, f.target, ".gitignore", "/forest\n/forest.prev\n/.forest/\n/.iron-forest/runtime/\n/.iron-forest/bin/\n")
	runGitDir(t, f.target, "add", "forest.yaml", ".gitignore")
	runGitDir(t, f.target, "commit", "-m", "coherent legacy source")
	f.priorSHA = strings.TrimSpace(string(runGitDir(t, f.target, "rev-parse", "HEAD")))
	runGitDir(t, f.target, "remote", "add", "origin", origin)
	runGitDir(t, f.target, "push", "origin", "HEAD:master")
	if err := os.WriteFile(filepath.Join(f.target, "forest"), []byte(f.binary), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTree(t, f.target, "forest.prev", "unrelated stale backup\n")
	writeTree(t, f.target, ".forest/runs.jsonl", f.evidence)
	writeTree(t, f.home, ".config/systemd/user/forest@.service", "prior unit\n")
	writeTree(t, f.home, ".config/iron-forest/inst.env", "OPENROUTER_API_KEY=test-only\n")
	if err := os.Chmod(filepath.Join(f.home, ".config/iron-forest/inst.env"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The fetched revision carries the complete source profile migration. The
	// deployment transaction alone adopts the historical runtime and binary.
	author := filepath.Join(temp, "author")
	runGit(t, "clone", origin, author)
	configGit(t, author, "Deploy Test", "deploy@forest.invalid")
	if err := os.MkdirAll(filepath.Join(author, profileName), 0o755); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, author, "mv", "forest.yaml", profileName+"/config.yaml")
	runGitDir(t, author, "commit", "-m", "profile cutover")
	runGitDir(t, author, "push", "origin", "HEAD:master")

	systemctl := `#!/bin/sh
case "$2" in
  stop)
    printf 'stop\n' >> "$DEPLOY_LOG"
    while [ ! -f "$DEPLOY_RELEASE" ]; do sleep 0.01; done
    printf inactive > "$DEPLOY_STATE"
    ;;
  is-active) cat "$DEPLOY_STATE" ;;
  restart) printf 'restart\n' >> "$DEPLOY_LOG"; printf active > "$DEPLOY_STATE" ;;
esac
`
	mise := `#!/bin/sh
printf 'build\n' >> "$DEPLOY_LOG"
[ "$DEPLOY_FAILURE" != build ] || exit 71
previous=''
for arg in "$@"; do
  [ "$previous" != -o ] || destination="$arg"
  previous="$arg"
done
cat > "$destination" <<'FOREST'
#!/bin/sh
case "$1" in
  version)
    sha="$DEPLOY_FACTORY_SHA"
    [ "$DEPLOY_FAILURE" != version ] || sha=wrong
    printf '{"exit":0,"data":{"build_sha":"%s"}}\n' "$sha"
    ;;
  selfcheck) [ "$DEPLOY_FAILURE" != selfcheck ] || exit 72 ;;
  audit) printf '{"exit":0,"data":{"last_result":"pass","last_at":"2026-01-01T00:00:00Z"}}\n' ;;
  status) printf '{"exit":0,"data":{"audit":{"last_result":"pass","last_at":"2026-01-01T00:00:00Z"}}}\n' ;;
  admission) printf '{"exit":0,"data":{"paused":true,"drained":true}}\n' ;;
esac
FOREST
chmod +x "$destination"
`
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	git := `#!/bin/sh
case "$*" in
  *"fetch origin"*) printf 'fetch\n' >> "$DEPLOY_LOG"; [ "$DEPLOY_FAILURE" != fetch ] || exit 73 ;;
esac
exec "$DEPLOY_REAL_GIT" "$@"
`
	for name, body := range map[string]string{"systemctl": systemctl, "mise": mise, "git": git} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	state := filepath.Join(temp, "service-state")
	if err := os.WriteFile(state, []byte("active"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	f.command = exec.CommandContext(ctx, "bash", filepath.Join(f.factory, "deploy/install-service.sh"), "update", "inst", f.factorySHA)
	f.command.Dir = temp
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "HOME=") && !strings.HasPrefix(value, "PATH=") {
			f.command.Env = append(f.command.Env, value)
		}
	}
	f.command.Env = append(f.command.Env, "HOME="+f.home, "PATH="+bin+":"+os.Getenv("PATH"), "DEPLOY_LOG="+f.log, "DEPLOY_RELEASE="+f.release, "DEPLOY_STATE="+state, "DEPLOY_REAL_GIT="+realGit, "DEPLOY_FAILURE="+failure, "DEPLOY_FACTORY_SHA="+f.factorySHA)
	f.command.Stdout, f.command.Stderr = &f.output, &f.output
	return f
}

func TestDeployUpdateDrainsBeforeProceeding(t *testing.T) {
	f := newDeployFixture(t, "")
	if err := f.command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(f.release, nil, 0o644) })
	waitForLogLine(t, f.log, "stop")
	events, _ := os.ReadFile(f.log)
	if strings.Contains(string(events), "fetch") || strings.Contains(string(events), "build") {
		t.Fatalf("deployment changed source before drain completed: %s", events)
	}
	if got := string(mustReadFile(t, filepath.Join(f.target, "forest"))); got != f.binary {
		t.Fatalf("binary changed during drain: %s", got)
	}
	if err := os.WriteFile(f.release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.command.Wait(); err != nil {
		t.Fatalf("adoption failed: %v\n%s", err, f.output.String())
	}
	if _, err := os.Stat(filepath.Join(f.target, profileName, "bin", "forest")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(configPath(f.target)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.target, "forest")); !os.IsNotExist(err) {
		t.Fatalf("legacy executable survived adoption: %v", err)
	}
	if got := string(mustReadFile(t, ledgerPath(f.target))); got != f.evidence {
		t.Fatalf("adoption changed historical evidence: %s", got)
	}
}

func TestDeployUpdateRollbackUsesOnlyCurrentTransaction(t *testing.T) {
	for _, stage := range []string{"fetch", "build", "selfcheck", "version"} {
		t.Run(stage, func(t *testing.T) {
			f := newDeployFixture(t, stage)
			if err := os.WriteFile(f.release, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := f.command.Run(); err == nil {
				t.Fatalf("%s failure succeeded: %s", stage, f.output.String())
			}
			if got := string(mustReadFile(t, filepath.Join(f.target, "forest"))); got != f.binary {
				t.Fatalf("restored stale or candidate binary: %s\n%s", got, f.output.String())
			}
			if got := strings.TrimSpace(string(runGitDir(t, f.target, "rev-parse", "HEAD"))); got != f.priorSHA {
				t.Fatalf("source/binary rollback split: %s want %s", got, f.priorSHA)
			}
			if got := string(mustReadFile(t, filepath.Join(f.target, ".forest/runs.jsonl"))); got != f.evidence {
				t.Fatalf("rollback lost evidence: %s", got)
			}
			if got := string(mustReadFile(t, filepath.Join(f.target, "forest.prev"))); got != "unrelated stale backup\n" {
				t.Fatalf("transaction consumed unrelated backup: %s", got)
			}
			if got := string(mustReadFile(t, filepath.Join(f.home, ".config/systemd/user/forest@.service"))); got != "prior unit\n" {
				t.Fatalf("rollback retained candidate service: %s", got)
			}
			if _, err := os.Stat(filepath.Join(f.target, "forest.yaml")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(configPath(f.target)); !os.IsNotExist(err) {
				t.Fatalf("candidate profile survived rollback: %v", err)
			}
		})
	}
}

func TestDeployUpdateSiblingRejectsStaleFactoryRevision(t *testing.T) {
	f := newDeployFixture(t, "")
	writeTree(t, f.factory, "advanced", "new source\n")
	runGitDir(t, f.factory, "add", "advanced")
	runGitDir(t, f.factory, "commit", "-m", "not adopted")
	advanced := strings.TrimSpace(string(runGitDir(t, f.factory, "rev-parse", "HEAD")))
	runGitDir(t, f.factory, "checkout", f.factorySHA)
	f.command.Args[len(f.command.Args)-1] = advanced
	if err := f.command.Run(); err == nil {
		t.Fatal("stale factory was deployed")
	}
	if _, err := os.Stat(f.log); !os.IsNotExist(err) {
		t.Fatalf("consumer was touched before factory revision fence: %v\n%s", err, f.output.String())
	}
}

func TestDeployUpdateRejectsCompetingRuntimeOwnersBeforeTouchingConsumer(t *testing.T) {
	f := newDeployFixture(t, "")
	writeTree(t, f.target, profileName+"/runtime/runs.jsonl", "current runtime evidence\n")
	if err := f.command.Run(); err == nil {
		t.Fatal("competing legacy/current runtime owners were adopted")
	}
	if _, err := os.Stat(f.log); !os.IsNotExist(err) {
		t.Fatalf("consumer was touched before runtime ownership was resolved: %v\n%s", err, f.output.String())
	}
	if got := string(mustReadFile(t, ledgerPath(f.target))); got != "current runtime evidence\n" {
		t.Fatalf("current runtime evidence changed: %q", got)
	}
	if got := string(mustReadFile(t, filepath.Join(f.target, ".forest/runs.jsonl"))); got != f.evidence {
		t.Fatalf("legacy runtime evidence changed: %q", got)
	}
}

func waitForLogLine(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(body), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q in %s", want, path)
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
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }
