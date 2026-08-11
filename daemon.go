package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// version is stamped at build time with -ldflags "-X main.version=<sha>".

var version = "dev"

// factoryDir is the factory's own source checkout, declared by the operator
// with `serve --factory-dir`. It is empty by default: a managed repository is
// not the factory's source, and building it would swap in a foreign binary.
var factoryDir string

var updateGate sync.RWMutex

var selfUpdateTicker = func() (<-chan time.Time, func()) {
	ticker := time.NewTicker(60 * time.Second)
	return ticker.C, ticker.Stop
}
var selfUpdateCheck = runUpdateCheck
var exitSelf = os.Exit

type updateRecord struct {
	Time   string `json:"time"`
	From   string `json:"from_sha"`
	To     string `json:"to_sha"`
	Status string `json:"status"`
}

func canonicalLocalPath(source string) string {
	absolute, err := filepath.Abs(source)
	if err != nil {
		absolute = source
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return absolute
}

func factorySourceLockPath(source string) string {
	absolute := canonicalLocalPath(source)
	root := filepath.Join("/tmp", fmt.Sprintf("iron-forest-%d", os.Getuid()), "factory-source")
	return filepath.Join(root, blobSHA(absolute)+".lock")
}
func selfUpdateLoop(repoDir string, drain *int32, drainNow <-chan struct{}) {
	if factoryDir == "" {
		return
	}
	ticks, stop := selfUpdateTicker()
	defer stop()
	for {
		select {
		case <-drainNow:
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
		}
		if atomic.LoadInt32(drain) != 0 {
			return
		}
		updateGate.Lock()
		if atomic.LoadInt32(drain) == 0 {
			selfUpdateCheck(repoDir, drain)
		}
		updateGate.Unlock()
	}
}

// runUpdateCheck rebuilds this instance from the factory source. Source and
// managed repository are separate, so the checkout being worked on is never
// built and may be in any language.
func runUpdateCheck(repoDir string, drain *int32) {
	sourceLock, err := holdLock(factorySourceLockPath(factoryDir))
	if err != nil {
		if !errors.Is(err, errAdmissionHeld) {
			fmt.Fprintf(os.Stderr, "forest: update: source lock: %v\n", err)
		}
		return
	}
	defer dropLock(sourceLock)
	if err := removeInterruptedUpdateArtifacts(repoDir); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: remove interrupted artifacts: %v\n", err)
		return
	}
	// Only the instance that manages the factory source may move it. Every
	// other instance rebuilds from whatever that source currently is, so one
	// process mutates the shared checkout and all of them still converge.
	if canonicalLocalPath(repoDir) == canonicalLocalPath(factoryDir) {
		ref, err := gitOut(factoryDir, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil || ref != "master" {
			fmt.Fprintf(os.Stderr, "forest: update: not on master (%s), skipping\n", ref)
			return
		}
		if err := git(factoryDir, "fetch", "--quiet", "origin", "master"); err != nil {
			fmt.Fprintf(os.Stderr, "forest: update: fetch: %v\n", err)
			return
		}
		head, err := gitOut(factoryDir, "rev-parse", "HEAD")
		if err != nil {
			fmt.Fprintf(os.Stderr, "forest: update: rev-parse HEAD: %v\n", err)
			return
		}
		remote, err := gitOut(factoryDir, "rev-parse", "origin/master")
		if err != nil {
			fmt.Fprintf(os.Stderr, "forest: update: rev-parse origin/master: %v\n", err)
			return
		}
		if head != remote {
			if deferDirty(factoryDir) {
				return
			}
			if err := git(factoryDir, "pull", "--ff-only", "origin", "master"); err != nil {
				fmt.Fprintf(os.Stderr, "forest: update: pull: %v\n", err)
				return
			}
		}
	}
	short, err := gitOut(factoryDir, "rev-parse", "--short", "HEAD")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: rev-parse --short HEAD: %v\n", err)
		return
	}
	if version == short {
		return
	}
	if deferDirty(factoryDir) {
		return
	}
	if err := buildSelf(factoryDir, repoDir, short); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: build: %v\n", err)
		return
	}
	// The smoke test runs in the managed checkout: a binary that starts but
	// cannot read this repository's own config must not be swapped in.
	fresh := filepath.Join(repoDir, "forest.next")
	smoke := exec.Command(fresh, "selfcheck")
	smoke.Dir = repoDir
	if out, err := runCombinedOutput(smoke); err != nil {
		_ = os.Remove(fresh)
		fmt.Fprintf(os.Stderr, "forest: update: selfcheck failed, keeping current build:\n%s", out)
		return
	}
	// The signal path never waits here. If this check wins the race, install
	// can finish; otherwise drain removes the tested binary and stops the update.
	if draining(drain) {
		_ = os.Remove(fresh)
		return
	}
	swapSelf(repoDir, short, version, short)
}

func deferDirty(repoDir string) bool {
	dirty, err := gitOut(repoDir, "status", "--porcelain")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: status: %v\n", err)
		return true
	}
	if dirty == "" {
		return false
	}
	lines := strings.SplitN(dirty, "\n", 5)

	suffix := ""
	if len(lines) == 5 {
		suffix = "…"
	}
	fmt.Fprintf(os.Stderr, "forest: update: working tree not clean, deferring:\n%s%s\n",
		strings.Join(lines, "\n"), suffix)
	return true
}
func removeInterruptedUpdateArtifacts(repoDir string) error {
	var errs []error
	for _, name := range []string{"forest.prev", "forest.next", "forest.next.tmp"} {
		err := os.Remove(filepath.Join(repoDir, name))
		if err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func buildSelf(srcDir, outDir, shortSHA string) error {
	tmp := filepath.Join(outDir, "forest.next.tmp")
	target := filepath.Join(outDir, "forest.next")
	defer os.Remove(tmp)
	ldflags := "-X main.version=" + shortSHA
	try := func(name string, args ...string) error {
		c := exec.Command(name, args...)
		c.Dir = srcDir
		out, err := runCombinedOutput(c)
		if err != nil {
			return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
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

func swapSelf(repoDir, shortSHA, fromSHA, toSHA string) {
	cur := filepath.Join(repoDir, "forest")
	next := filepath.Join(repoDir, "forest.next")
	if err := os.Rename(next, cur); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: install new binary: %v\n", err)
		return
	}
	if err := appendUpdate(repoDir, updateRecord{
		Time: nowRFC(), From: fromSHA, To: toSHA, Status: "swapped",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: record: %v\n", err)
	}
	fmt.Fprintf(os.Stderr, "forest: self-updated to %s; exiting for supervisor restart\n", shortSHA)
	exitSelf(0)
}

// acquireSingletonLock takes one non-blocking lock for a running service.
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
		return nil, fmt.Errorf("%s/daemon.lock is held by another forest service", WorkspaceDir)
	}
	return f, nil
}

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

// cmdSelfcheck verifies config, every declaration, and every configured Flow
// agent offline. An unused malformed declaration is still invalid repository
// state: `forest agents` can discover it, so selfcheck must reject it.
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
	if _, err := readProviderConfig(repoDir); err != nil {
		fmt.Fprintln(os.Stderr, "forest selfcheck:", err)
		return 1
	}
	names, err := discoverAgents(repoDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest selfcheck:", err)
		return 1
	}
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "forest selfcheck: no agents under agents/")
		return 1
	}
	declared := make(map[string]bool, len(names))
	for _, name := range names {
		if _, err := loadAgent(repoDir, name); err != nil {
			fmt.Fprintf(os.Stderr, "forest selfcheck: agent %s: %v\n", name, err)
			return 1
		}
		declared[name] = true
	}
	for _, name := range []string{cfg.Flows.Builder.Agent, cfg.Flows.Verifier.Agent, cfg.Flows.Fixer.Agent, cfg.Flows.Manager.Agent} {
		if name != "" && !declared[name] {
			fmt.Fprintf(os.Stderr, "forest selfcheck: configured agent %s has no declaration\n", name)
			return 1
		}
	}
	if err := verifyChildSandbox(repoDir); err != nil {
		fmt.Fprintln(os.Stderr, "forest selfcheck:", err)
		return 1
	}
	fmt.Printf("forest %s selfcheck: ok\n", version)
	return 0
}

func hasBin(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}
