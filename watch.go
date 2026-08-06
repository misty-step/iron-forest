package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// cmdWatch draws a live operator board over the factory's on-disk state.
// Default frame is local-only (JSONL + git HEAD + systemd) so a 2s refresh
// never hammers GitHub. Pass --live-gh to also poll the backlog via gh.
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
	fs.BoolVar(&liveGH, "live-gh", false, "poll the GitHub backlog")
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

	// Optional slow backlog refresh so --live-gh does not call gh every frame.
	var (
		mu      sync.Mutex
		items   []issue
		itemErr string
	)
	if liveGH {
		refresh := func() {
			got, err := backlog(cfg)
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
	LiveGH     bool
	Backlog    []issue
	BacklogErr string
	OpenPRs    []prState
	Recent     []runRecord
	Updates    []updateRecord
	Stats      watchStats
}

type daemonSnap struct {
	Active bool
	PID    string
	Unit   string
	Note   string
}

type watchStats struct {
	Runs   int
	Cost   float64
	Done   int
	Failed int
	Fixed  int
	Other  int
}

func loadWatchSnapshot(cfg Config, repoDir string) watchSnapshot {
	ws := filepath.Join(repoDir, WorkspaceDir)
	s := watchSnapshot{
		DrawnAt: time.Now().UTC(),
		Repo:    cfg.Repo,
		Version: version,
	}
	if h, err := gitOut(repoDir, "rev-parse", "--short", "HEAD"); err == nil {
		s.HeadShort = h
	}
	s.Daemon = probeDaemon(repoDir)
	s.OpenPRs = latestOpenPRs(ws)
	all, _, _ := loadLedger(filepath.Join(ws, "runs.jsonl"))
	s.Recent = tailRuns(all, 8)
	s.Updates = tailUpdates(ws, 5)
	s.Stats = summarizeRuns(all)
	return s
}

func probeDaemon(repoDir string) daemonSnap {
	d := daemonSnap{Unit: "forest-chew.service"}
	out, err := exec.Command("systemctl", "--user", "is-active", "forest-chew").Output()
	active := err == nil && strings.TrimSpace(string(out)) == "active"
	d.Active = active
	if active {
		if pid, err := exec.Command("systemctl", "--user", "show", "forest-chew", "-p", "MainPID", "--value").Output(); err == nil {
			d.PID = strings.TrimSpace(string(pid))
		}
		d.Note = "systemd --user"
		return d
	}
	// Fallback: lock held means some chew is running outside systemd.
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

func latestOpenPRs(workspace string) []prState {
	b, err := os.ReadFile(filepath.Join(workspace, "prs.jsonl"))
	if err != nil {
		return nil
	}
	last := map[int]prState{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s prState
		if json.Unmarshal([]byte(line), &s) != nil || s.PR == 0 {
			continue
		}
		last[s.PR] = s
	}
	out := make([]prState, 0, len(last))
	for _, s := range last {
		switch s.State {
		case "merged", "closed":
			if t, err := time.Parse(time.RFC3339, s.Time); err == nil && time.Since(t) > 48*time.Hour {
				continue
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := prActiveRank(out[i].State), prActiveRank(out[j].State)
		if ai != aj {
			return ai < aj
		}
		return out[i].PR > out[j].PR
	})
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func prActiveRank(state string) int {
	switch state {
	case "fixing":
		return 0
	case "ready":
		return 1
	case "opened":
		return 2
	case "stalled":
		return 3
	case "merged":
		return 4
	default:
		return 5
	}
}

func tailRuns(all []runRecord, n int) []runRecord {
	if len(all) > n {
		return all[len(all)-n:]
	}
	return all
}

func tailUpdates(workspace string, n int) []updateRecord {
	f, err := os.Open(filepath.Join(workspace, "updates.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []updateRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var u updateRecord
		if json.Unmarshal(line, &u) != nil {
			continue
		}
		all = append(all, u)
	}
	if len(all) > n {
		return all[len(all)-n:]
	}
	return all
}

func summarizeRuns(all []runRecord) watchStats {
	var st watchStats
	for _, r := range all {
		st.Runs++
		st.Cost += r.CostUSD
		switch runCategory(r.Status) {
		case "done":
			st.Done++
		case "fixed":
			st.Fixed++
		case "failed":
			st.Failed++
		default:
			st.Other++
		}
	}
	return st
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

	fmt.Fprintf(w, "LEDGER  runs=%d  done=%d  fixed=%d  failed=%d  other=%d  cost=$%.4f\n",
		s.Stats.Runs, s.Stats.Done, s.Stats.Fixed, s.Stats.Failed, s.Stats.Other, s.Stats.Cost)
	fmt.Fprintln(w, strings.Repeat("─", 78))

	if s.LiveGH {
		fmt.Fprintf(w, "BACKLOG (%d)  source=gh every 30s\n", len(s.Backlog))
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
				fmt.Fprintf(w, "  #%-5d %s\n", it.Number, title)
			}
		}
	} else {
		fmt.Fprintln(w, "BACKLOG  (local frame — pass --live-gh to poll GitHub)")
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "PRS (latest state, active first)")
	if len(s.OpenPRs) == 0 {
		fmt.Fprintln(w, "  (none recorded)")
	} else {
		fmt.Fprintf(w, "  %-5s %-10s %-8s %-8s %5s %s\n", "PR", "STATE", "OWL", "CHECKS", "FIXES", "ISSUE")
		for _, p := range s.OpenPRs {
			fmt.Fprintf(w, "  #%-4d %-10s %-8s %-8s %5d #%d\n",
				p.PR, trunc(p.State, 10), trunc(orDash(p.Owl), 8), trunc(orDash(p.Checks), 8), p.Fixes, p.Issue)
		}
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "RECENT RUNS")
	if len(s.Recent) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		for i := len(s.Recent) - 1; i >= 0; i-- {
			r := s.Recent[i]
			t := r.Time
			if len(t) >= 19 {
				t = t[11:19]
			}
			agent := r.Agent
			if agent == "" {
				agent = "-"
			}
			if len(agent) > 16 {
				agent = agent[:15] + "…"
			}
			fmt.Fprintf(w, "  %s  #%-4d %-16s %-14s $%.4f  %s\n",
				t, r.Issue, trunc(r.Status, 16), agent, r.CostUSD, trunc(orDash(r.ReviewVerdict), 8))
		}
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "SELF-UPDATES")
	if len(s.Updates) == 0 {
		fmt.Fprintln(w, "  (none)")
	} else {
		for i := len(s.Updates) - 1; i >= 0; i-- {
			u := s.Updates[i]
			t := u.Time
			if len(t) >= 19 {
				t = t[:19]
			}
			fmt.Fprintf(w, "  %s  %s → %s  %s\n", t, shortSHA(u.From), shortSHA(u.To), u.Status)
		}
	}
	fmt.Fprintln(w, strings.Repeat("─", 78))
	fmt.Fprintln(w, "sources: .forest/*.jsonl  git HEAD  systemd --user forest-chew")
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
