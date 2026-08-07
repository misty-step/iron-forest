package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// A Subject is the one thing a flow acts on in a pass. Key stays stable across
// revisions of the same work. Revision is what the flow actually saw: an item's
// update stamp, or a branch's head commit. A flow that recorded a decision
// against a revision must not decide again until the revision moves.
type Subject struct {
	Key      string // "item-41", "branch-forest/41-add-notes"
	Kind     string // item | branch
	Revision string // item updatedAt, or branch head sha
	Label    string // one line for the operator
	ID       string // tracker item identity, opaque to the controller
	Item     Item   // Kind == "item"
	Branch   string // Kind == "branch"
	Head     string // Kind == "branch": head commit sha
}

// An Outcome is what one Act call did. It becomes one ledger row. There is no
// money in it: spend is bounded by the provider key, not counted here.
type Outcome struct {
	Status  string // done | reviewed | merged | fixed | skipped | <stage>_failed
	Branch  string
	PRURL   string
	Verdict string
	Agent   string
	Model   string
	DefSHA  string
	// BaseSHA is the commit the run actually acted on. It differs from the
	// Subject's Revision when the run moved the branch first, as a rebase does:
	// Revision says why the lane woke, BaseSHA says what it checked and merged.
	BaseSHA string
	TokIn   int64
	TokOut  int64
	Err     error
}

// A Flow is one autonomous lane. It reads observable state, acts, and records.
// Flows never call each other: the repository is the only interface between
// them, so one flow's effect is another flow's selector match.
type Flow interface {
	// Name is the flow's word in the glossary and in the ledger.
	Name() string
	// Select returns the subjects this flow would act on right now, most
	// deserving first. It must be a pure read with no writes.
	Select(cfg Config, repoDir string) ([]Subject, error)
	// Act performs the flow's declared effects on one subject.
	Act(cfg Config, repoDir string, s Subject, runID string) (Outcome, error)
	// Interval is how long the flow sleeps between passes.
	Interval(cfg Config) time.Duration
	// Enabled reports whether the operator declared this flow on.
	Enabled(cfg Config) bool
}

// flowsFor builds the declared flows. Adding a lane is adding one entry here
// plus one file: the supervisor and the ledger need no change.
func flowsFor() []Flow {
	return []Flow{builderFlow{}, verifierFlow{}, fixerFlow{}}
}

// codeBusy is a pass's answer when another worker already handles the subject.
// It is not a failure: nothing was built or decided, so the flow moves on.
const codeBusy = 2

// serve runs every enabled flow, each in its own goroutine on its own clock.
// names filters to a subset of flows; empty means every enabled flow.
func serve(cfg Config, repoDir string, names []string) int {
	lock, err := acquireSingletonLock(repoDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", err)
		return 1
	}
	defer lock.Close()

	// A worktree leaked by an abnormal exit is reaped once, before any flow
	// starts, so a stale run directory never survives a restart.
	reapOrphanWorktrees(repoDir)

	var drain int32
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig // first signal: finish the in-flight agents, start no new pass
		atomic.StoreInt32(&drain, 1)
		fmt.Fprintln(os.Stderr, "forest: draining, waiting for in-flight agents")
		<-sig // second signal: the operator's only clock, since agents are unbounded
		fmt.Fprintln(os.Stderr, "forest: second signal, exiting now")
		for _, dir := range trackedWorktrees() {
			removeWorktree(repoDir, dir)
			fmt.Fprintf(os.Stderr, "forest: removed in-flight worktree %s\n", dir)
		}
		os.Exit(1)
	}()

	selected := flowsFor()
	if len(names) > 0 {
		var keep []Flow
		for _, f := range selected {
			for _, n := range names {
				if f.Name() == n {
					keep = append(keep, f)
				}
			}
		}
		selected = keep
		if len(selected) == 0 {
			fmt.Fprintf(os.Stderr, "forest: no such flow: %v\n", names)
			return 2
		}
	}
	var live []Flow
	for _, f := range selected {
		if f.Enabled(cfg) {
			live = append(live, f)
		}
	}
	if len(live) == 0 {
		fmt.Fprintln(os.Stderr, "forest: every flow is disabled")
		return 1
	}

	fmt.Printf("forest v%s: %s on %s\n", version, flowNames(live), cfg.Repo)
	// The self-updater is not a lane: it owns no subject. It swaps the binary
	// only when no subject is in flight, so a rebuild never kills a live agent.
	go selfUpdateLoop(cfg, repoDir, &drain)

	var wg sync.WaitGroup
	for _, f := range live {
		wg.Add(1)
		go func(f Flow) {
			defer wg.Done()
			runFlowLoop(f, cfg, repoDir, &drain)
		}(f)
	}
	wg.Wait()
	return 0
}

func flowNames(fs []Flow) string {
	out := ""
	for i, f := range fs {
		if i > 0 {
			out += " + "
		}
		out += f.Name()
	}
	return out
}

// runFlowLoop is one lane's whole life: select, act, record, sleep. Config is
// re-read every pass so an operator edit lands without a restart, and a failing
// pass never stops the lane.
func runFlowLoop(f Flow, cfg Config, repoDir string, drain *int32) {
	for {
		if atomic.LoadInt32(drain) == 1 {
			fmt.Fprintf(os.Stderr, "forest: %s draining, no new pass\n", f.Name())
			return
		}
		if nc, err := loadConfig(configPath(repoDir)); err == nil {
			cfg = nc
		}
		if !f.Enabled(cfg) {
			time.Sleep(f.Interval(cfg))
			continue
		}
		if code := runFlowPass(f, cfg, repoDir); code == 0 {
			// A pass that did work re-selects immediately: the state it wrote
			// may have made another subject actionable for this same lane.
			continue
		}
		time.Sleep(f.Interval(cfg))
	}
}

// runFlowPass acts on at most one subject and reports 0 when it did work.
// One subject per pass keeps a lane's decisions small and re-reads the world
// between them, so a lane never acts on state it has already invalidated.
func runFlowPass(f Flow, cfg Config, repoDir string) int {
	subjects, err := f.Select(cfg, repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: %s select: %v\n", f.Name(), err)
		return 1
	}
	if len(subjects) == 0 {
		return 1
	}
	for _, s := range subjects {
		code := actOnSubject(f, cfg, repoDir, s)
		if code == codeBusy {
			continue // another worker handles it; try the next candidate
		}
		return code
	}
	return 1
}

// actOnSubject excludes one subject within this process, acts, and records.
// The read-side update gate spans Act so a binary swap cannot interrupt it.
// Act must never take this lock again: a second read lock behind a waiting
// writer deadlocks.
func actOnSubject(f Flow, cfg Config, repoDir string, s Subject) int {
	updateGate.RLock()
	defer updateGate.RUnlock()
	if !inFlight.claim(s.Key) {
		return codeBusy
	}
	defer inFlight.release(s.Key)

	runID := fmt.Sprintf("%s-%s", time.Now().UTC().Format("20060102T150405Z"), s.Key)

	fmt.Printf("forest: %s %s\n", f.Name(), s.Label)
	out, err := f.Act(cfg, repoDir, s, runID)
	rec := runRecord{
		Time: nowRFC(), RunID: runID, Flow: f.Name(), Subject: s.Key,
		Revision: s.Revision, ID: s.ID, Branch: out.Branch, PRURL: out.PRURL,
		Status: out.Status, TokensIn: out.TokIn, TokOut: out.TokOut,
		Agent: out.Agent, Model: out.Model, DefSHA: out.DefSHA,
		BaseSHA: out.BaseSHA, ReviewVerdict: out.Verdict,
	}
	if err != nil {
		if brakeErr := recordStalled(repoDir, f.Name(), s.Key, s.Revision); brakeErr != nil {
			err = fmt.Errorf("%w; record stalled: %v", err, brakeErr)
		}
		if rec.Status == "" || rec.Status == "done" {
			rec.Status = failStatus(err)
		}
		rec.Error = err.Error()
		_ = appendRun(workspaceDir(repoDir), rec)
		fmt.Fprintf(os.Stderr, "forest: %s %s: %v\n", f.Name(), s.Key, err)
		return 1
	}
	_ = appendRun(workspaceDir(repoDir), rec)
	fmt.Printf("forest: %s %s %s\n", f.Name(), s.Key, rec.Status)
	return 0
}

// runOnce acts on one named subject with one flow, outside the daemon. It does
// not take the singleton lock, so the in-process subject exclusion remains the
// only guard for a manual dispatch beside a running daemon.
func runOnce(cfg Config, repoDir, flowName, subject string) int {
	for _, f := range flowsFor() {
		if f.Name() != flowName {
			continue
		}
		subjects, err := f.Select(cfg, repoDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "forest:", err)
			return 1
		}
		for _, s := range subjects {
			if s.Key == subject || s.Branch == subject ||
				(s.ID != "" && s.ID == subject) {
				return actOnSubject(f, cfg, repoDir, s)
			}
		}
		fmt.Fprintf(os.Stderr, "forest: %s does not select %q now\n", flowName, subject)
		for _, s := range subjects {
			fmt.Fprintf(os.Stderr, "  candidate: %s\n", s.Key)
		}
		return 1
	}
	fmt.Fprintf(os.Stderr, "forest: no such flow: %s\n", flowName)
	return 2
}

// failStatus maps a stage-prefixed error to the ledger's failure vocabulary.
func failStatus(err error) string {
	if err == nil {
		return "done"
	}
	for _, stage := range []string{"agent", "gate", "review", "prompt", "checks",
		"worktree", "publish", "merge", "notes"} {
		if strings.HasPrefix(err.Error(), stage+":") {
			return stage + "_failed"
		}
	}
	return "flow_failed"
}
