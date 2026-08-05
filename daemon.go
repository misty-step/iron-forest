package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// version is stamped at build time with -ldflags "-X main.version=<sha>".
// It identifies which merge this binary was built from; see `forest version`.
var version = "dev"

// updateRecord is one self-update row in .forest/updates.jsonl: when the
// factory swapped itself to a newer build of itself.
type updateRecord struct {
	Time   string `json:"time"`
	From   string `json:"from_sha"`
	To     string `json:"to_sha"`
	Status string `json:"status"`
}

// runUpdateCheck pulls green master and, when a new build exists, swaps the
// running process to the fresh binary by exec. It runs only at a pass
// boundary, never while an agent is in flight. Systemd never restarts us: the
// exec keeps the same PID, so the singleton lock and the service state stay
// continuous across the swap.
func runUpdateCheck(repoDir string) {
	cfgPath := filepath.Join(repoDir, "forest.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		return // not a forest clone; do not try to update
	}
	ref, err := gitOut(repoDir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || ref != "master" {
		fmt.Fprintf(os.Stderr, "forest: update: not on master (%s), skipping\n", ref)
		return
	}
	if err := git(repoDir, "fetch", "--quiet", "origin", "master"); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: fetch: %v\n", err)
		return
	}
	head, err := gitOut(repoDir, "rev-parse", "HEAD")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: rev-parse HEAD: %v\n", err)
		return
	}
	remote, err := gitOut(repoDir, "rev-parse", "origin/master")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: rev-parse origin/master: %v\n", err)
		return
	}
	if head == remote {
		return // already running the latest merged build
	}
	dirty, err := gitOut(repoDir, "status", "--porcelain")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: status: %v\n", err)
		return
	}
	if dirty != "" {
		// Deferral must never be silent: say exactly what blocks the pull so
		// a stuck daemon is diagnosable from its log alone.
		head := strings.SplitN(dirty, "\n", 5)
		ellipsis := ""
		if len(head) == 5 {
			ellipsis = "…"
		}
		fmt.Fprintf(os.Stderr, "forest: update: working tree not clean, deferring:\n%s%s\n",
			strings.Join(head, "\n"), ellipsis)
		return
	}
	if err := git(repoDir, "pull", "--ff-only", "origin", "master"); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: pull: %v\n", err)
		return
	}
	short, err := gitOut(repoDir, "rev-parse", "--short", "HEAD")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: rev-parse --short HEAD: %v\n", err)
		return
	}
	if err := buildSelf(repoDir, short); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: build: %v\n", err)
		return
	}
	fresh := filepath.Join(repoDir, "forest.next")
	smoke := exec.Command(fresh, "selfcheck")
	smoke.Dir = repoDir
	if out, err := smoke.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: selfcheck failed, keeping current build:\n%s", out)
		return
	}
	_ = appendUpdate(repoDir, updateRecord{Time: nowRFC(), From: head, To: remote, Status: "swapped"})
	swapSelf(repoDir, short)
}

// buildSelf compiles the current checkout into forest.next. It prefers the
// project toolchain (mise) when plain `go` is not on PATH, so the same build
// works inside the daemon and in a dev shell.
func buildSelf(repoDir, shortSHA string) error {
	tmp := filepath.Join(repoDir, "forest.next.tmp")
	target := filepath.Join(repoDir, "forest.next")
	ldflags := "-X main.version=" + shortSHA
	try := func(name string, args ...string) error {
		c := exec.Command(name, args...)
		c.Dir = repoDir
		out, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v: %s",
				name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	var err error
	switch {
	case hasBin("go"):
		err = try("go", "build", "-o", tmp, "-ldflags", ldflags, ".")
	case hasBin("mise"):
		err = try("mise", "exec", "--", "go", "build", "-o", tmp, "-ldflags", ldflags, ".")
	default:
		return fmt.Errorf("no go toolchain on PATH (tried go and mise)")
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

// swapSelf installs forest.next as forest and execs it, replacing the current
// process image. On any failure it restores the previous binary and keeps
// running with the old image.
func swapSelf(repoDir, shortSHA string) {
	cur := filepath.Join(repoDir, "forest")
	prev := filepath.Join(repoDir, "forest.prev")
	next := filepath.Join(repoDir, "forest.next")
	if err := os.Rename(cur, prev); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "forest: update: keep current: %v\n", err)
		return
	}
	if err := os.Rename(next, cur); err != nil {
		_ = os.Rename(prev, cur)
		fmt.Fprintf(os.Stderr, "forest: update: install new binary: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "forest: self-updated to %s, handoff at next pass\n", shortSHA)
	argv := append([]string{cur}, os.Args[1:]...)
	if err := syscall.Exec(cur, argv, os.Environ()); err != nil {
		_ = os.Rename(cur, next)
		_ = os.Rename(prev, cur)
		fmt.Fprintf(os.Stderr, "forest: update: exec: %v\n", err)
	}
}

// acquireSingletonLock takes an exclusive, non-blocking flock on
// .forest/daemon.lock so only one `forest chew` runs at a time. The lock fd
// is CLOEXEC, so self-exec drops it and the fresh process image re-acquires
// it at the next chewLoop start — a microsecond gap owned by the same pid.
func acquireSingletonLock(repoDir string) (*os.File, error) {
	dir := filepath.Join(repoDir, WorkspaceDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s/daemon.lock is held by another forest daemon", WorkspaceDir)
	}
	return f, nil
}

// appendUpdate records one self-swap in the deploy ledger.
func appendUpdate(repoDir string, r updateRecord) error {
	dir := filepath.Join(repoDir, WorkspaceDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "updates.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(r)
}

// cmdSelfcheck is the smoke gate the updater runs on a freshly built binary
// before swapping to it. It must be cheap and offline: config parses, agents
// load, and the workflow names resolve to real agents.
func cmdSelfcheck(repoDir string) int {
	cfgPath := filepath.Join(repoDir, "forest.yaml")
	if _, err := os.Stat(cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "forest selfcheck: no forest.yaml here")
		return 1
	}
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest selfcheck:", err)
		return 1
	}
	for _, name := range []string{cfg.Workflow.Build, cfg.Workflow.Review} {
		if name == "" {
			continue
		}
		if _, err := loadAgent(repoDir, name); err != nil {
			fmt.Fprintf(os.Stderr, "forest selfcheck: agent %s: %v\n", name, err)
			return 1
		}
	}
	if names, err := discoverAgents(repoDir); err != nil {
		fmt.Fprintln(os.Stderr, "forest selfcheck:", err)
		return 1
	} else if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "forest selfcheck: no agents under agents/")
		return 1
	}
	fmt.Printf("forest %s selfcheck: ok\n", version)
	return 0
}

func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
