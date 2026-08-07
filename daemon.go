package main

import (
	"encoding/json"
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

func recordPendingUpdate() {
	from, to := os.Getenv("FOREST_UPDATE_FROM"), os.Getenv("FOREST_UPDATE_TO")
	if from == "" || to == "" {
		return
	}
	_ = os.Unsetenv("FOREST_UPDATE_FROM")
	_ = os.Unsetenv("FOREST_UPDATE_TO")
	repoDir, err := os.Getwd()
	if err != nil {
		return
	}
	if err := appendUpdate(repoDir, updateRecord{Time: nowRFC(), From: from, To: to, Status: "swapped"}); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: record: %v\n", err)
	}
}

var version = "dev"

// factoryDir is the factory's own source checkout, declared by the operator
// with `serve --factory-dir`. It is empty by default: a managed repository is
// not the factory's source, and building it would swap in a foreign binary.
var factoryDir string

var updateGate sync.RWMutex

type updateRecord struct {
	Time   string `json:"time"`
	From   string `json:"from_sha"`
	To     string `json:"to_sha"`
	Status string `json:"status"`
}

// selfUpdateLoop checks for a new binary only when all lanes are idle.
func selfUpdateLoop(cfg Config, repoDir string, drain *int32) {
	_ = cfg
	if factoryDir == "" {
		return
	}
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if atomic.LoadInt32(drain) != 0 {
			return
		}
		if !inFlight.idle() {
			continue
		}
		updateGate.Lock()
		if inFlight.idle() {
			runUpdateCheck(repoDir)
		}
		updateGate.Unlock()
	}
}

// runUpdateCheck rebuilds this instance from the factory source. Source and
// managed repository are separate, so the checkout being worked on is never
// built and may be in any language.
func runUpdateCheck(repoDir string) {
	// Only the instance that manages the factory source may move it. Every
	// other instance rebuilds from whatever that source currently is, so one
	// process mutates the shared checkout and all of them still converge.
	if repoDir == factoryDir {
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
	if out, err := smoke.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "forest: update: selfcheck failed, keeping current build:\n%s", out)
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

func buildSelf(srcDir, outDir, shortSHA string) error {
	tmp := filepath.Join(outDir, "forest.next.tmp")
	target := filepath.Join(outDir, "forest.next")
	ldflags := "-X main.version=" + shortSHA
	try := func(name string, args ...string) error {
		c := exec.Command(name, args...)
		c.Dir = srcDir
		out, err := c.CombinedOutput()
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
	if err := os.Setenv("FOREST_UPDATE_FROM", fromSHA); err != nil {
		_ = os.Rename(cur, next)
		_ = os.Rename(prev, cur)
		fmt.Fprintf(os.Stderr, "forest: update: pending record: %v\n", err)
		return
	}
	if err := os.Setenv("FOREST_UPDATE_TO", toSHA); err != nil {
		_ = os.Unsetenv("FOREST_UPDATE_FROM")
		_ = os.Rename(cur, next)
		_ = os.Rename(prev, cur)
		fmt.Fprintf(os.Stderr, "forest: update: pending record: %v\n", err)
		return
	}
	fmt.Fprintf(os.Stderr, "forest: self-updated to %s, handoff at next pass\n", shortSHA)
	argv := append([]string{cur}, os.Args[1:]...)
	if err := syscall.Exec(cur, argv, os.Environ()); err != nil {
		_ = os.Unsetenv("FOREST_UPDATE_FROM")
		_ = os.Unsetenv("FOREST_UPDATE_TO")
		_ = os.Rename(cur, next)
		_ = os.Rename(prev, cur)
		fmt.Fprintf(os.Stderr, "forest: update: exec: %v\n", err)
	}
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

// cmdSelfcheck verifies config and every configured lane agent offline.
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
	for _, name := range []string{cfg.Flows.Builder.Agent, cfg.Flows.Verifier.Agent, cfg.Flows.Fixer.Agent, cfg.Flows.Manager.Agent} {
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
