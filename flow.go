package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type subjectKind string

const (
	subjectItem       subjectKind = "item"
	subjectBranch     subjectKind = "branch"
	subjectRetirement subjectKind = "retirement"
)

// A Subject is the one thing a flow acts on in a pass. Key stays stable across
// revisions of the same work. Revision is what the flow actually saw: an item's
// update stamp, or a branch's head commit. A flow that recorded a decision
// against a revision must not decide again until the revision moves.
type Subject struct {
	Key      string // "item-41", "branch-forest/41-add-notes"
	Kind     subjectKind
	Revision string // item update stamp or branch head commit
	Label    string // one line for the operator
	ID       string // tracker item identity, opaque to the controller
	Item     Item   // subjectItem or subjectRetirement
	Branch   string // subjectBranch or subjectRetirement
	Head     string // branch head commit
}

// An Outcome is what one Act call did. It becomes one ledger row. There is no
// money in it: spend is bounded by the provider key, not counted here.
type Outcome struct {
	Status  string // done | reviewed | merged | fixed | skipped | blocked | <stage>_failed
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
	// CacheRead and CacheWrite are cached input tokens, billed at a different
	// rate than fresh input; Reasoning is the reasoning-class token spend. Each
	// is a measured class the ledger must record, not sum into one figure.
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Err        error
}

// addTokens carries every token class a phase measured onto the outcome. A
// class that stops being copied here is the discard defect the ledger exists to
// prevent, so every field is copied explicitly.
func (o *Outcome) addTokens(s runStats) {
	o.TokIn = s.tokensIn
	o.TokOut = s.tokensOut
	o.CacheRead = s.cacheRead
	o.CacheWrite = s.cacheWrite
	o.Reasoning = s.reasoning
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
	return []Flow{builderFlow{}, verifierFlow{}, fixerFlow{}, managerFlow{}}
}

// codeBusy is a pass's answer when another worker already handles the subject.
// It is not a failure: nothing was built or decided, so the flow moves on.
const codeBusy = 2

// shutdownStatus is the ledger status for a run the operator stopped: the
// daemon is draining, so the agent's exit is a shutdown, not a crash. It is
// deliberately not a *_failed status so it never reaches the repeat-failure
// brake. Tokens already spent are still recorded on the row.
const shutdownStatus = "shutdown"

// draining reports whether the daemon received the graceful signal and is
// shutting down. A nil drain means no daemon (a manual runOnce), where there is
// nothing to distinguish a shutdown from a crash.
func draining(drain *int32) bool {
	return drain != nil && atomic.LoadInt32(drain) == 1
}

// serve runs every enabled flow, each in its own goroutine on its own clock.
func serve(cfg Config, repoDir string, names []string) int {
	return serveSelected(cfg, repoDir, names, flowsFor())
}

func serveSelected(cfg Config, repoDir string, names []string, selected []Flow) int {
	lock, err := acquireSingletonLock(repoDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest:", redactSecretShaped(err.Error()))
		return 1
	}
	defer lock.Close()

	// A worktree leaked by an abnormal exit is reaped once, before any flow
	// starts, so a stale run directory never survives a restart.
	if err := removeInterruptedUpdateArtifacts(repoDir); err != nil {
		fmt.Fprintf(os.Stderr, "forest: remove interrupted update artifacts: %s\n", redactSecretShaped(err.Error()))
	}
	reapOrphanWorktrees(repoDir)

	var drain int32
	drainNow := make(chan struct{})
	served := make(chan struct{})
	defer close(served)
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		select {
		case <-sig:
		case <-served:
			return
		}
		// Drain starts immediately, even when final update I/O is blocked. An
		// update whose last drain check already passed may finish its install;
		// both paths stop this process, and a second signal always stays live.
		atomic.StoreInt32(&drain, 1)
		close(drainNow)
		fmt.Fprintln(os.Stderr, "forest: draining, waiting for in-flight agents")
		select {
		case <-sig:
		case <-served:
			return
		}
		fmt.Fprintln(os.Stderr, "forest: second signal, exiting now")
		for _, err := range hardStopRunCommands() {
			fmt.Fprintln(os.Stderr, "forest:", redactSecretShaped(err.Error()))
		}
		// A forced stop does not wait for blocked repository I/O. The next
		// startup reaps every linked worktree before it starts a Flow.
		os.Exit(1)
	}()

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
			fmt.Fprintf(os.Stderr, "forest: no such flow: %s\n", redactSecretShaped(fmt.Sprint(names)))
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
	// The updater takes the write gate only between Subject actions.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		selfUpdateLoop(repoDir, &drain, drainNow)
	}()
	for _, f := range live {
		wg.Add(1)
		go func(f Flow) {
			defer wg.Done()
			runFlowLoop(f, cfg, repoDir, &drain, drainNow)
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

// hostConfigName is the factory's own composition file. It is the one source of
// truth for the checks: the runner executes every declared check on the host
// through sh -c (see runner.go), so it is the only file whose contents an agent
// wants to change to gain host execution on the next pass.
const hostConfigName = "forest.yaml"

// verifyHostConfig is the authority boundary that gates the host config against
// check injection (#119). A run executes only inside a linked worktree, so any
// working-tree change to the factory's own forest.yaml is a command installed
// out of band: the next pass would re-read it (see runFlowLoop) and run the
// altered checks: on the host, on a path that survives the worktree's removal.
// The factory therefore refuses to let any flow act while the host forest.yaml
// differs from the committed revision. The one legitimate way to change what the
// runner executes is to commit the change — so the diff is empty — and let it
// arrive through independent review and merge. On a mismatch it returns an error
// naming the file, which the pass loop reports and uses to skip acting.
func verifyHostConfig(repoDir string) error {
	out, err := gitOutRaw(repoDir, "status", "--porcelain", "--", hostConfigName)
	if err != nil {
		return fmt.Errorf("cannot verify host config %s: %w", hostConfigName, err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("host config %s was modified outside the worktree; refusing to act (git diff HEAD -- %s must be empty)", hostConfigName, hostConfigName)
	}
	return nil
}

// runFlowLoop is one lane's whole life: select, act, record, sleep. Config is
// re-read every pass so an operator edit lands without a restart, and a failing
// pass never stops the lane.
//
// The immediate re-select after productive work exists so a lane can pick up
// sibling work its own write unblocked. It is only valid for *different* work:
// if a pass acts on the same subject it just acted on, the lane is not making
// progress and must wait. Without that rule any action that succeeds while
// changing nothing becomes a hot loop, which is what 217 identical verifier
// passes on one branch were.
func runFlowLoop(f Flow, cfg Config, repoDir string, drain *int32, drainNow <-chan struct{}) {
	var lastKey string
	for {
		if atomic.LoadInt32(drain) == 1 {
			fmt.Fprintf(os.Stderr, "forest: %s draining, no new pass\n", f.Name())
			return
		}
		nc, err := loadConfig(configPath(repoDir))
		if err != nil {
			fmt.Fprintf(os.Stderr, "forest: %s config: %s\n", f.Name(), redactSecretShaped(err.Error()))
			if !waitFlowInterval(f.Interval(cfg), drainNow) {
				return
			}
			continue
		}
		cfg = nc
		if !f.Enabled(cfg) {
			if !waitFlowInterval(f.Interval(cfg), drainNow) {
				return
			}
			continue
		}
		if err := verifyHostConfig(repoDir); err != nil {
			// The host config was modified outside this lane: a run left a
			// working-tree write in forest.yaml, which would change what the next
			// pass's checks execute on the host. Refuse to act this pass, name the
			// file, and retry on the next interval so an operator sees it.
			fmt.Fprintf(os.Stderr, "forest: %s: %s\n", f.Name(), redactSecretShaped(err.Error()))
			if !waitFlowInterval(f.Interval(cfg), drainNow) {
				return
			}
			continue
		}
		code, key := runFlowPass(f, cfg, repoDir, drain)
		if code == 0 && key != lastKey {
			// This pass did work on a subject it did not just handle: its write
			// may have made another subject actionable, so re-select at once.
			lastKey = key
			continue
		}
		lastKey = key
		if !waitFlowInterval(f.Interval(cfg), drainNow) {
			return
		}
	}
}

func waitFlowInterval(interval time.Duration, drainNow <-chan struct{}) bool {
	if drainNow == nil {
		time.Sleep(interval)
		return true
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-drainNow:
		return false
	}
}

// runFlowPass acts on at most one subject and reports 0 when it did work, plus
// the key of the subject it acted on so the caller can tell repeated work from
// progress. One subject per pass keeps a lane's decisions small and re-reads the
// world between them, so a lane never acts on state it has already invalidated.
func runFlowPass(f Flow, cfg Config, repoDir string, drain *int32) (int, string) {
	subjects, err := f.Select(cfg, repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: %s select: %s\n", f.Name(), redactSecretShaped(err.Error()))
		return 1, ""
	}
	if len(subjects) == 0 {
		return 1, ""
	}
	for _, s := range subjects {
		code := actOnSubject(f, cfg, repoDir, s, drain)
		if code == codeBusy {
			continue // another worker handles it; try the next candidate
		}
		return code, s.Key
	}
	return 1, ""
}

var (
	runPrefix   = fmt.Sprintf("%s-%d", time.Now().UTC().Format("20060102T150405.000000000Z"), os.Getpid())
	runSequence uint64
)

func newRunID() string {
	return fmt.Sprintf("%s-%016x", runPrefix, atomic.AddUint64(&runSequence, 1))
}

// actOnSubject admits one Subject across every process and checkout before it
// spends work, then acts and records. The read-side update gate spans Act so a
// binary swap cannot interrupt it. Act must never take this lock again: a
// second read lock behind a waiting writer deadlocks.
func actOnSubject(f Flow, cfg Config, repoDir string, s Subject, drain *int32) int {
	updateGate.RLock()
	defer updateGate.RUnlock()
	if draining(drain) {
		return 1
	}

	runID := newRunID()
	release, err := claimAdmission(repoDir, cfg.Repo, f.Name(), s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: %s %s: %s\n", f.Name(), redactSecretShaped(s.Key), redactSecretShaped(err.Error()))
		if errors.Is(err, errAdmissionHeld) {
			return codeBusy
		}
		return 1
	}
	defer release()

	fmt.Printf("forest: %s %s\n", f.Name(), redactSecretShaped(s.Label))
	out, err := f.Act(cfg, repoDir, s, runID)
	rec := runRecord{
		Time: nowRFC(), RunID: runID, Flow: f.Name(), Subject: s.Key,
		Revision: s.Revision, ID: s.ID, Branch: out.Branch, PRURL: out.PRURL,
		Status: out.Status, TokensIn: out.TokIn, TokOut: out.TokOut,
		Agent: out.Agent, Model: out.Model, DefSHA: out.DefSHA,
		BaseSHA: out.BaseSHA, ReviewVerdict: out.Verdict,
	}
	rec.setTokens(out)
	if err != nil {
		if draining(drain) {
			// The operator stopped the daemon; the agent exited because of that,
			// not because of its own work. Name the status so it never reads as a
			// failure, keep the spent tokens, and leave the brake untouched.
			rec.Status = shutdownStatus
		} else {
			var brakeErr error
			switch {
			case s.Kind != subjectRetirement:
				brakeErr = recordStalled(repoDir, f.Name(), s.Key, s.Revision)
			case errors.Is(err, errRetirementStale):
				brakeErr = recordTerminalStall(repoDir, f.Name(), s.Key, s.Revision)
			}
			if brakeErr != nil {
				err = fmt.Errorf("%w; record stalled: %v", err, brakeErr)
			}
			if rec.Status == "" || rec.Status == "done" {
				rec.Status = failStatus(err)
			}
		}
		rec.Error = redactSecretShaped(err.Error())
		if ledgerErr := appendRun(workspaceDir(repoDir), rec); ledgerErr != nil {
			fmt.Fprintf(os.Stderr, "forest: %s %s ledger: %s\n", f.Name(), redactSecretShaped(s.Key), redactSecretShaped(ledgerErr.Error()))
			return 1
		}
		fmt.Fprintf(os.Stderr, "forest: %s %s: %s\n", f.Name(), redactSecretShaped(s.Key), redactSecretShaped(err.Error()))
		return 1
	}
	if err := appendRun(workspaceDir(repoDir), rec); err != nil {
		fmt.Fprintf(os.Stderr, "forest: %s %s ledger: %s\n", f.Name(), redactSecretShaped(s.Key), redactSecretShaped(err.Error()))
		return 1
	}
	fmt.Printf("forest: %s %s %s\n", f.Name(), redactSecretShaped(s.Key), redactSecretShaped(rec.Status))
	return 0
}

// runOnce acts on one named Subject with one Flow outside the daemon. The
// durable admission in actOnSubject prevents a manual run from duplicating
// work held by serve or another checkout.
func runOnce(cfg Config, repoDir, flowName, subject string) int {
	if err := verifyHostConfig(repoDir); err != nil {
		fmt.Fprintln(os.Stderr, "forest:", redactSecretShaped(err.Error()))
		return 1
	}
	for _, f := range flowsFor() {
		if f.Name() != flowName {
			continue
		}
		subjects, err := f.Select(cfg, repoDir)
		if err != nil {
			fmt.Fprintln(os.Stderr, "forest:", redactSecretShaped(err.Error()))
			return 1
		}
		match, found, err := resolveSelectedSubject(subjects, subject)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forest: %s: %s\n", redactSecretShaped(flowName), redactSecretShaped(err.Error()))
			return 1
		}
		if found {
			return actOnSubject(f, cfg, repoDir, match, nil)
		}
		fmt.Fprintf(os.Stderr, "forest: %s does not select %q now\n", redactSecretShaped(flowName), redactSecretShaped(subject))
		for _, s := range subjects {
			fmt.Fprintf(os.Stderr, "  candidate: %s\n", redactSecretShaped(s.Key))
		}
		return 1
	}
	fmt.Fprintf(os.Stderr, "forest: no such flow: %s\n", redactSecretShaped(flowName))
	return 2
}

func resolveSelectedSubject(subjects []Subject, name string) (Subject, bool, error) {
	var matches []Subject
	for _, s := range subjects {
		if s.Key == name || s.Branch == name || (s.ID != "" && s.ID == name) {
			matches = append(matches, s)
		}
	}
	if len(matches) == 0 {
		return Subject{}, false, nil
	}
	if len(matches) > 1 {
		keys := make([]string, len(matches))
		for i, s := range matches {
			keys[i] = s.Key
		}
		return Subject{}, false, fmt.Errorf("subject %q is ambiguous across %s", name, strings.Join(keys, ", "))
	}
	return matches[0], true, nil
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
