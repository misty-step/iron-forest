package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// cmdWatch draws a live operator board from the ledger, refs, and daemon.
// The default frame is local-only. Pass --live-gh to poll the tracker backlog.
// Ctrl-C restores the cursor and exits.
func cmdWatch(cfg Config, repoDir string, args []string) int {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "forest watch [--interval 2s] [--live-gh]  live board over .forest/ + daemon")
	}
	interval := 2 * time.Second
	liveGH := false
	fs.DurationVar(&interval, "interval", interval, "refresh interval (min 200ms)")
	fs.DurationVar(&interval, "n", interval, "refresh interval (min 200ms)")
	fs.BoolVar(&liveGH, "live-gh", false, "poll the tracker backlog")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "forest: watch: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if interval < 200*time.Millisecond {
		fmt.Fprintf(os.Stderr, "forest: watch: bad interval %q\n", interval)
		return 2
	}

	fmt.Fprint(os.Stdout, "\033[?25l")
	defer fmt.Fprint(os.Stdout, "\033[?25h\033[0m")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	var (
		mu      sync.Mutex
		items   []Item
		itemErr string
	)
	if liveGH {
		refresh := func() {
			got, err := eligibleItems(cfg, repoDir)
			mu.Lock()
			if err != nil {
				itemErr = err.Error()
				items = nil
			} else {
				itemErr = ""
				items = got
			}
			mu.Unlock()
		}
		refresh()
		go func() {
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for range t.C {
				refresh()
			}
		}()
	}

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		snap := loadWatchSnapshot(cfg, repoDir)
		if liveGH {
			mu.Lock()
			snap.Backlog = items
			snap.BacklogErr = itemErr
			snap.LiveGH = true
			mu.Unlock()
		}
		fmt.Fprint(os.Stdout, "\033[H\033[2J")
		renderWatch(os.Stdout, snap)
		select {
		case <-tick.C:
		case <-sig:
			fmt.Fprintln(os.Stdout)
			return 0
		}
	}
}

// watchSnapshot is one frame of operator-visible factory state.
type watchSnapshot struct {
	DrawnAt    time.Time
	Repo       string
	Version    string
	HeadShort  string
	Daemon     daemonSnap
	Worktrees  []string
	LiveGH     bool
	Backlog    []Item
	BacklogErr string
	Flows      map[string][]runRecord
}

type daemonSnap struct {
	Active bool
	PID    string
	Unit   string
	Note   string
}

func loadWatchSnapshot(cfg Config, repoDir string) watchSnapshot {
	ws := filepath.Join(repoDir, WorkspaceDir)
	s := watchSnapshot{
		DrawnAt: time.Now().UTC(),
		Repo:    cfg.Repo,
		Version: version,
		Flows:   map[string][]runRecord{"builder": nil, "verifier": nil, "fixer": nil},
	}
	if h, err := gitOut(repoDir, "rev-parse", "--short", "HEAD"); err == nil {
		s.HeadShort = h
	}
	s.Daemon = probeDaemon(repoDir)
	all, _, _ := loadLedger(filepath.Join(ws, "runs.jsonl"))
	s.Flows = groupRuns(all, 8)
	s.Worktrees = worktreePaths(repoDir)
	return s
}

func probeDaemon(repoDir string) daemonSnap {
	d := daemonSnap{Unit: "forest.service"}
	out, err := exec.Command("systemctl", "--user", "is-active", "forest").Output()
	active := err == nil && strings.TrimSpace(string(out)) == "active"
	d.Active = active
	if active {
		if pid, err := exec.Command("systemctl", "--user", "show", "forest", "-p", "MainPID", "--value").Output(); err == nil {
			d.PID = strings.TrimSpace(string(pid))
		}
		d.Note = "systemd --user"
		return d
	}
	lock := filepath.Join(repoDir, WorkspaceDir, "daemon.lock")
	if f, err := os.Open(lock); err == nil {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		_ = f.Close()
		if err != nil {
			d.Active = true
			d.Note = "daemon.lock held (not via systemd?)"
			return d
		}
	}
	d.Note = "inactive"
	return d
}

func groupRuns(all []runRecord, n int) map[string][]runRecord {
	groups := map[string][]runRecord{
		"builder":  nil,
		"verifier": nil,
		"fixer":    nil,
	}
	for _, r := range all {
		name := strings.ToLower(strings.TrimSpace(r.Flow))
		if name != "builder" && name != "verifier" && name != "fixer" {
			continue
		}
		groups[name] = append(groups[name], r)
	}
	for name, runs := range groups {
		if len(runs) > n {
			groups[name] = runs[len(runs)-n:]
		}
	}
	return groups
}

func worktreePaths(repoDir string) []string {
	out, err := gitOut(repoDir, "worktree", "list", "--porcelain")
	if err != nil || out == "" {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if path := strings.TrimPrefix(line, "worktree "); path != line && path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func renderWatch(w *os.File, s watchSnapshot) {
	now := s.DrawnAt.Format("15:04:05Z")
	fmt.Fprintf(w, "iron-forest watch  %s  repo=%s\n", now, s.Repo)
	fmt.Fprintf(w, "binary=%-10s  HEAD=%-10s  refresh=live  Ctrl-C quit\n", s.Version, orDash(s.HeadShort))
	fmt.Fprintln(w, strings.Repeat("─", 78))

	dstate := "DOWN"
	if s.Daemon.Active {
		dstate = "UP"
	}
	fmt.Fprintf(w, "DAEMON  %s  unit=%s  pid=%s  %s\n", dstate, s.Daemon.Unit, orDash(s.Daemon.PID), s.Daemon.Note)
	fmt.Fprintln(w)

	fmt.Fprintf(w, "WORKTREES (%d)  source=git worktree list --porcelain\n", len(s.Worktrees))
	if len(s.Worktrees) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		for _, path := range s.Worktrees {
			fmt.Fprintf(w, "  %s\n", path)
		}
	}
	fmt.Fprintln(w)

	if s.LiveGH {
		fmt.Fprintf(w, "BACKLOG (%d)  source=tracker every 30s\n", len(s.Backlog))
		if s.BacklogErr != "" {
			fmt.Fprintf(w, "  error: %s\n", s.BacklogErr)
		} else if len(s.Backlog) == 0 {
			fmt.Fprintln(w, "  (empty)")
		} else {
			for _, it := range s.Backlog {
				title := it.Title
				if len(title) > 56 {
					title = title[:53] + "..."
				}
				fmt.Fprintf(w, "  %-6s %s\n", orDash(it.ID), title)
			}
		}
	} else {
		fmt.Fprintln(w, "BACKLOG  (local frame — pass --live-gh to poll the tracker)")
	}
	fmt.Fprintln(w)

	for _, flow := range []string{"builder", "verifier", "fixer"} {
		runs, ok := s.Flows[flow]
		if !ok {
			continue
		}
		fmt.Fprintf(w, "%s (%d recent runs)\n", strings.ToUpper(flow), len(runs))
		if len(runs) == 0 {
			fmt.Fprintln(w, "  (none)")
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintf(w, "  %-8s %-24s %-10s %-14s %s\n", "TIME", "SUBJECT", "REVISION", "STATUS", "AGENT")
		for i := len(runs) - 1; i >= 0; i-- {
			r := runs[i]
			t := r.Time
			if len(t) >= 19 {
				t = t[11:19]
			}
			fmt.Fprintf(w, "  %-8s %-24s %-10s %-14s %s\n", t, trunc(orDash(r.Subject), 24),
				shortSHA(r.Revision), trunc(orDash(r.Status), 14), trunc(orDash(r.Agent), 20))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, strings.Repeat("─", 78))
	fmt.Fprintln(w, "sources: .forest/runs.jsonl  git worktree list --porcelain  git HEAD  .forest/daemon.lock  systemd --user forest.service")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	if s == "" {
		return "-"
	}
	return s
}
