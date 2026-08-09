package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmdArgs := args
	if dir != "" {
		cmdArgs = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", cmdArgs...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", cmdArgs, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

func startTestProcess(t *testing.T, cmd *exec.Cmd) (<-chan error, func()) {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			<-done
		}
	})
	return done, func() { waited = true }
}

func newRefGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "repo")
	runGitTest(t, root, "init", "--bare", remote)
	runGitTest(t, root, "init", repo)
	runGitTest(t, repo, "remote", "add", "origin", remote)
	return repo
}

// setupTestRepo builds a throwaway repository with a real origin, because
// createWorktree resolves its base from the remote tip and a fixture without a
// remote would prove nothing about the path the flows actually take.
func setupTestRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	repo := filepath.Join(root, "work")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, origin, "init", "--bare", "-b", "master")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "init", "-b", "master")
	runGitTest(t, repo, "config", "user.email", "test@example.com")
	runGitTest(t, repo, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "file.txt")
	runGitTest(t, repo, "commit", "-m", "init")
	runGitTest(t, repo, "remote", "add", "origin", origin)
	runGitTest(t, repo, "push", "-q", "-u", "origin", "master")
	return repo
}

func notesTestRepository(t *testing.T) (remote, work, sha string) {
	t.Helper()
	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	work = filepath.Join(root, "work")
	runGitTest(t, "", "init", "--bare", "--initial-branch=master", remote)
	runGitTest(t, "", "clone", remote, work)
	runGitTest(t, work, "config", "user.name", "notes-test")
	runGitTest(t, work, "config", "user.email", "notes-test@example.com")
	if err := os.WriteFile(filepath.Join(work, "file.txt"), []byte("first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, work, "add", "file.txt")
	runGitTest(t, work, "commit", "-m", "first")
	runGitTest(t, work, "push", "-u", "origin", "HEAD:master")
	sha = runGitTest(t, work, "rev-parse", "HEAD")
	return remote, work, sha
}

func newAdmissionRepositories(t *testing.T) (repoA, repoB string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repoA = filepath.Join(root, "a")
	repoB = filepath.Join(root, "b")
	runGitTest(t, root, "init", "--bare", "--initial-branch=master", remote)
	runGitTest(t, root, "clone", remote, repoA)
	runGitTest(t, root, "clone", remote, repoB)
	return repoA, repoB
}

func writeAgentFixture(t *testing.T, repoDir, name, model string) {
	t.Helper()
	dir := filepath.Join(repoDir, DefaultAgentsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := "description: " + name + "\ncommit:\n  name: " + name + "\n  email: " + name + "@example.invalid\nmodel: " + model + "\ndeadline_seconds: 3600\n"
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "instructions.md"), []byte("do the work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("{{.Task}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.schema.json"), []byte("{\"type\":\"object\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rebaseTestWriteFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testVerifierAgent() *Agent {
	return &Agent{
		Name: "verifier", Model: "verifier-model", DefSHA: strings.Repeat("a", 16),
		Commit: CommitIdentity{Name: "forest-test", Email: "forest-test@example.com"},
	}
}

// remoteBranchHead reads the head of one branch advertised by origin.
func remoteBranchHead(t *testing.T, repo, branch string) string {
	t.Helper()
	out := runGitTest(t, repo, "ls-remote", "origin", "refs/heads/"+branch)
	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("origin branch %q not found", branch)
	}
	return fields[0]
}

// memoryTracker is an in-memory tracker used by tests. Open items live in the
// map; closing one removes it, exactly as a host would stop returning it.
type memoryTracker struct {
	items map[string]Item
}

// newMemoryTracker returns an empty in-memory tracker.
func newMemoryTracker() *memoryTracker {
	return &memoryTracker{items: make(map[string]Item)}
}

// seed inserts or replaces one contract-valid item by id. Tests that exercise
// revision validation use a raw Tracker stub instead of this behavioral fake.
func (m *memoryTracker) seed(it Item) {
	if it.UpdatedAt == "" {
		it.UpdatedAt = "test-revision"
	}
	m.items[it.ID] = it
}

// ListOpen implements Tracker.
func (m *memoryTracker) ListOpen() ([]Item, error) {
	items := make([]Item, 0, len(m.items))
	for _, it := range m.items {
		items = append(items, it)
	}
	return items, nil
}

// Get implements Tracker.
func (m *memoryTracker) Get(id string) (Item, error) {
	it, ok := m.items[id]
	if !ok {
		return Item{}, fmt.Errorf("item %q not found", id)
	}
	return it, nil
}

// Comment implements Tracker.
func (m *memoryTracker) Comment(id, body string) error {
	it, err := m.Get(id)
	if err != nil {
		return err
	}
	it.Comments = append(it.Comments, comment{Body: body})
	m.items[id] = it
	return nil
}

// Close implements Tracker. Closing an absent item is idempotent because a
// recovery can retry cleanup after an earlier Host close succeeded.
func (m *memoryTracker) Close(id string) error {
	delete(m.items, id)
	return nil
}

// SetTags implements Tracker.
func (m *memoryTracker) SetTags(id string, add, remove []string) error {
	it, err := m.Get(id)
	if err != nil {
		return err
	}
	tags := make(map[string]bool, len(it.Tags)+len(add))
	for _, t := range it.Tags {
		tags[t] = true
	}
	for _, t := range remove {
		delete(tags, t)
	}
	for _, t := range add {
		tags[t] = true
	}
	it.Tags = it.Tags[:0]
	for t := range tags {
		it.Tags = append(it.Tags, t)
	}
	m.items[id] = it
	return nil
}
func listRetirements(repoDir string) ([]retirementFact, error) {
	facts, err := scanRetirements(repoDir)
	if err != nil {
		return nil, err
	}
	for _, fact := range facts {
		if fact.ReadErr != nil {
			return nil, fact.ReadErr
		}
	}
	return facts, nil
}
