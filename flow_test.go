package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// failingFlow is a Flow stub whose Act always fails like a crashed agent. Tests
// use it to drive actOnSubject without talking to a real tracker or opencode.
type failingFlow struct{}

func (failingFlow) Name() string                             { return "builder" }
func (failingFlow) Select(Config, string) ([]Subject, error) { return aSubject, nil }
func (failingFlow) Interval(Config) time.Duration            { return 0 }
func (failingFlow) Enabled(Config) bool                      { return true }
func (failingFlow) Act(Config, string, Subject, string) (Outcome, error) {
	return Outcome{TokIn: 7}, errAgentCrash
}

type terminalStaleFlow struct{ name string }

func (f terminalStaleFlow) Name() string                           { return f.name }
func (terminalStaleFlow) Select(Config, string) ([]Subject, error) { return nil, nil }
func (terminalStaleFlow) Interval(Config) time.Duration            { return 0 }
func (terminalStaleFlow) Enabled(Config) bool                      { return true }
func (terminalStaleFlow) Act(Config, string, Subject, string) (Outcome, error) {
	return Outcome{Status: "stale"}, errSubjectRevisionStale
}

type classifiedFailureFlow struct {
	name   string
	status string
	err    error
}

func (f classifiedFailureFlow) Name() string                           { return f.name }
func (classifiedFailureFlow) Select(Config, string) ([]Subject, error) { return nil, nil }
func (classifiedFailureFlow) Interval(Config) time.Duration            { return 0 }
func (classifiedFailureFlow) Enabled(Config) bool                      { return true }
func (f classifiedFailureFlow) Act(Config, string, Subject, string) (Outcome, error) {
	return Outcome{Status: f.status}, f.err
}

type operatorRedactionFlow struct{ secret string }

func (operatorRedactionFlow) Name() string                             { return "builder" }
func (operatorRedactionFlow) Select(Config, string) ([]Subject, error) { return nil, nil }
func (operatorRedactionFlow) Interval(Config) time.Duration            { return 0 }
func (operatorRedactionFlow) Enabled(Config) bool                      { return true }
func (f operatorRedactionFlow) Act(Config, string, Subject, string) (Outcome, error) {
	return Outcome{}, errors.New("agent failed with " + f.secret)
}

type shutdownFlow struct{ drain *int32 }

func (shutdownFlow) Name() string                             { return "builder" }
func (shutdownFlow) Select(Config, string) ([]Subject, error) { return aSubject, nil }
func (shutdownFlow) Interval(Config) time.Duration            { return 0 }
func (shutdownFlow) Enabled(Config) bool                      { return true }
func (f shutdownFlow) Act(Config, string, Subject, string) (Outcome, error) {
	*f.drain = 1
	return Outcome{TokIn: 7}, errAgentCrash
}

type captureRunIDFlow struct{ got *string }

func (f captureRunIDFlow) Name() string                             { return "capture" }
func (f captureRunIDFlow) Select(Config, string) ([]Subject, error) { return nil, nil }
func (f captureRunIDFlow) Interval(Config) time.Duration            { return 0 }
func (f captureRunIDFlow) Enabled(Config) bool                      { return true }
func (f captureRunIDFlow) Act(_ Config, _ string, _ Subject, runID string) (Outcome, error) {
	*f.got = runID
	return Outcome{Status: "done"}, nil
}

type reloadConfigFlow struct {
	observed chan bool
	release  chan struct{}
	drain    *int32
	passes   int32
}

func (*reloadConfigFlow) Name() string                  { return "reload" }
func (*reloadConfigFlow) Interval(Config) time.Duration { return time.Millisecond }
func (*reloadConfigFlow) Enabled(Config) bool           { return true }
func (f *reloadConfigFlow) Select(cfg Config, _ string) ([]Subject, error) {
	pass := atomic.AddInt32(&f.passes, 1)
	f.observed <- cfg.Flows.Verifier.AutoMerge
	if pass == 1 {
		<-f.release
	} else {
		atomic.StoreInt32(f.drain, 1)
	}
	return nil, nil
}
func (*reloadConfigFlow) Act(Config, string, Subject, string) (Outcome, error) {
	panic("reloadConfigFlow has no Subjects")
}

type pendingLoopFlow struct {
	acted    chan string
	interval time.Duration
}

func (*pendingLoopFlow) Name() string                    { return "pending-loop" }
func (f *pendingLoopFlow) Interval(Config) time.Duration { return f.interval }
func (*pendingLoopFlow) Enabled(Config) bool             { return true }
func (*pendingLoopFlow) Select(Config, string) ([]Subject, error) {
	return []Subject{
		{Key: "pending-a", Kind: subjectBranch, Revision: "r1", Label: "pending A"},
		{Key: "pending-b", Kind: subjectBranch, Revision: "r1", Label: "pending B"},
	}, nil
}
func (f *pendingLoopFlow) Act(_ Config, _ string, s Subject, _ string) (Outcome, error) {
	f.acted <- s.Key
	return Outcome{Status: "merge_pending"}, nil
}

type gateHoldingFlow struct {
	ready    chan string
	releases map[string]chan struct{}
}

func (gateHoldingFlow) Name() string                             { return "gate-holder" }
func (gateHoldingFlow) Select(Config, string) ([]Subject, error) { return nil, nil }
func (gateHoldingFlow) Interval(Config) time.Duration            { return 0 }
func (gateHoldingFlow) Enabled(Config) bool                      { return true }
func (f gateHoldingFlow) Act(_ Config, _ string, s Subject, _ string) (Outcome, error) {
	f.ready <- s.Key
	<-f.releases[s.Key]
	return Outcome{Status: "done"}, nil
}

var errAgentCrash = errors.New("agent: agent exited \"signal: terminated\"")

// aSubject is the sole subject a failingFlow selects.
var aSubject = []Subject{{Key: "item-1", Revision: "rev-1", ID: "1"}}

func TestRunIDsAreFlatAndUnique(t *testing.T) {
	runID := newRunID()
	if filepath.Base(runID) != runID || strings.Contains(runID, "..") {
		t.Fatalf("run id %q is not one flat path segment", runID)
	}
	if next := newRunID(); next == runID {
		t.Fatalf("two runs received the same id %q", runID)
	}
}

func TestResolveSelectedSubjectRejectsOpaqueIDKeyCollision(t *testing.T) {
	subjects := []Subject{
		{Key: "item-2", ID: "2"},
		{Key: "item-item-2", ID: "item-2"},
	}
	if _, found, err := resolveSelectedSubject(subjects, "item-2"); err == nil || found {
		t.Fatalf("ambiguous opaque Item resolution = (found=%v, err=%v), want refusal", found, err)
	}
	got, found, err := resolveSelectedSubject(subjects, "item-item-2")
	if err != nil || !found || got.ID != "item-2" {
		t.Fatalf("unambiguous Subject key resolution = (%#v, %v, %v)", got, found, err)
	}
}

func TestActOnSubjectUsesGeneratedRunID(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	var got string
	s := Subject{Key: `item-/../../outside\trace?[x]`, Revision: "r1"}
	if code := actOnSubject(captureRunIDFlow{got: &got}, Config{Repo: admissionTestRepo}, repo, s, nil); code != 0 {
		t.Fatalf("actOnSubject code = %d, want 0", code)
	}
	if got == "" || filepath.Base(got) != got || strings.Contains(got, "..") {
		t.Fatalf("Act received unsafe run id %q", got)
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil || len(rows) != 1 || rows[0].RunID != got {
		t.Fatalf("Ledger run id = (%#v, %v), want %q", rows, err, got)
	}
}

// TestShutdownIsNotAnAgentFailure pins the operator-shutdown card: a run that
// ends while the daemon is draining records a distinct shutdown status, keeps
// the tokens spent, and never increments the repeat-failure brake.
func TestShutdownIsNotAnAgentFailure(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	var drain int32
	for range stalledRunLimit + 1 {
		drain = 0
		if code := actOnSubject(shutdownFlow{drain: &drain}, Config{}, repo, aSubject[0], &drain); code != 1 {
			t.Fatalf("draining failing run code = %d, want 1", code)
		}
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1]
	if last.Status != shutdownStatus {
		t.Errorf("draining run status = %q, want %q", last.Status, shutdownStatus)
	}
	if last.TokensIn != 7 {
		t.Errorf("draining run tokensIn = %d, want 7 (spend kept)", last.TokensIn)
	}
	stalled, err := stalledOn(repo, "builder", aSubject[0].Key, aSubject[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if stalled {
		t.Fatalf("shutdown reached the failure brake; it must not count")
	}
}

func TestActOnSubjectRefusesNewWorkAfterDrain(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	var got string
	var drain int32 = 1
	code := actOnSubject(captureRunIDFlow{got: &got}, Config{Repo: admissionTestRepo}, repo, aSubject[0], &drain)
	if code != 1 || got != "" {
		t.Fatalf("act after drain = (%d, %q), want refusal before Act", code, got)
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil || len(rows) != 0 {
		t.Fatalf("Ledger after drained refusal = (%#v, %v), want empty", rows, err)
	}
}

// TestAgentFailureStillCounts pins the other side of the boundary: a real
// non-zero agent exit, with no shutdown in progress, still records agent_failed
// and still drives the repeat-failure brake.
func TestAgentFailureStillCounts(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	for range stalledRunLimit {
		if code := actOnSubject(failingFlow{}, Config{}, repo, aSubject[0], nil); code != 1 {
			t.Fatalf("failing run code = %d, want 1", code)
		}
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	if rows[len(rows)-1].Status != "agent_failed" {
		t.Errorf("real failure status = %q, want agent_failed", rows[len(rows)-1].Status)
	}
	stalled, err := stalledOn(repo, "builder", aSubject[0].Key, aSubject[0].Revision)
	if err != nil {
		t.Fatal(err)
	}
	if !stalled {
		t.Fatalf("real failures did not reach the brake; they must count")
	}
}

func TestRetryableNoteTransportNeverBrakesRevision(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	revision := strings.Repeat("a", 40)
	subject := Subject{
		Key: "branch-forest/19-notes", Kind: subjectBranch, Revision: revision,
		ID: "19", Branch: "forest/19-notes",
	}
	flow := classifiedFailureFlow{
		name: "verifier", status: "notes_failed",
		err: flowNoteError(errors.New("temporary remote failure")),
	}
	for range stalledRunLimit + 1 {
		if code := actOnSubject(flow, Config{}, repo, subject, nil); code != 1 {
			t.Fatalf("retryable notes code = %d, want failure", code)
		}
	}
	if stalled, err := stalledOn(repo, flow.Name(), subject.Key, revision); err != nil || stalled {
		t.Fatalf("retryable notes brake = (%v, %v), want selectable Revision", stalled, err)
	}
}
func TestTrackerTransportNeverBrakesRevision(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	revision := strings.Repeat("c", 40)
	subject := Subject{
		Key: "item-21", Kind: subjectItem, Revision: revision, ID: "21",
	}
	flow := classifiedFailureFlow{
		name: "builder", status: "item_failed", err: errTrackerUnavailable,
	}
	for range stalledRunLimit + 1 {
		if code := actOnSubject(flow, Config{}, repo, subject, nil); code != 1 {
			t.Fatalf("retryable Tracker code = %d, want failure", code)
		}
	}
	if stalled, err := stalledOn(repo, flow.Name(), subject.Key, revision); err != nil || stalled {
		t.Fatalf("retryable Tracker brake = (%v, %v), want selectable Revision", stalled, err)
	}
}

func TestPreparingRetirementReviewFailureReachesBrake(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	revision := strings.Repeat("b", 40)
	subject := Subject{
		Key: "retirement-forest/20-review", Kind: subjectRetirement, Revision: revision,
		ID: "20", Branch: "forest/20-review",
	}
	flow := classifiedFailureFlow{
		name: "verifier", status: "review_failed", err: errAgentCrash,
	}
	for range stalledRunLimit {
		if code := actOnSubject(flow, Config{}, repo, subject, nil); code != 1 {
			t.Fatalf("prepared review code = %d, want failure", code)
		}
	}
	if stalled, err := stalledOn(repo, flow.Name(),
		retirementAgentSubjectKey(subject.Branch), revision); err != nil || !stalled {
		t.Fatalf("prepared agent-work brake = (%v, %v), want durable park", stalled, err)
	}
	if stalled, err := stalledOn(repo, flow.Name(), subject.Key, revision); err != nil || stalled {
		t.Fatalf("retirement recovery brake = (%v, %v), want Host observation selectable", stalled, err)
	}
}

// TestActOnSubjectImmediatelyBrakesStaleVerifierAndFixer proves the production
// admission boundary publishes terminal stale brakes for both branch lanes.
func TestActOnSubjectImmediatelyBrakesStaleVerifierAndFixer(t *testing.T) {
	for _, name := range []string{"verifier", "fixer"} {
		t.Run(name, func(t *testing.T) {
			_, repo, _ := notesTestRepository(t)
			revision := strings.Repeat("a", 40)
			subject := Subject{
				Key: "branch-forest/17-stale-" + name, Kind: subjectBranch, Revision: revision,
				ID: "17", Branch: "forest/17-stale-" + name,
			}
			if code := actOnSubject(terminalStaleFlow{name: name}, Config{}, repo, subject, nil); code != 1 {
				t.Fatalf("stale %s code = %d, want failure", name, code)
			}
			if stalled, err := stalledOn(repo, name, subject.Key, revision); err != nil || !stalled {
				t.Fatalf("stale %s brake = %v, %v; want immediate terminal brake", name, stalled, err)
			}
		})
	}
}

func TestActOnSubjectRedactsOperatorText(t *testing.T) {
	const secret = "sk-AAAAAAAAAAAAAAAA"
	_, repo, _ := notesTestRepository(t)
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	code := actOnSubject(operatorRedactionFlow{secret: secret}, Config{}, repo,
		Subject{Key: "item-1", Revision: "rev-1", Label: "#1 title " + secret}, nil)
	_ = outW.Close()
	_ = errW.Close()
	os.Stdout, os.Stderr = oldOut, oldErr
	out, _ := io.ReadAll(outR)
	stderr, _ := io.ReadAll(errR)
	if code != 1 {
		t.Fatalf("actOnSubject code = %d, want 1", code)
	}
	for name, body := range map[string][]byte{"stdout": out, "stderr": stderr} {
		if strings.Contains(string(body), secret) || !strings.Contains(string(body), secretRedacted) {
			t.Fatalf("%s operator text = %q, want marker without original", name, body)
		}
	}
	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil || len(rows) != 1 {
		t.Fatalf("Ledger rows = (%#v, %v), want one row", rows, err)
	}
	if strings.Contains(rows[0].Error, secret) || !strings.Contains(rows[0].Error, secretRedacted) {
		t.Fatalf("Ledger error = %q, want marker without original", rows[0].Error)
	}
}

// TestBuilderTimeoutFailureParksNotFixes drives #207's mechanical classification
// end to end. A run that exceeds its declared wall-clock deadline is a
// mechanical failure: the same run keeps exceeding the same declared bound, so
// it must be named timeout_failed for an operator — never treated as a rejected
// change — and must never become a Fixer subject that spends a repair attempt on
// an unchanged situation. The builder flow fails inside runPhase before it ever
// publishes a branch, so nothing on origin offers the head to the Fixer and the
// attempt counter stays at zero.
func TestBuilderTimeoutFailureParksNotFixes(t *testing.T) {
	repo := setupTestRepo(t)
	writeAgentFixture(t, repo, "builder", "builder-model")

	tk := newMemoryTracker()
	tk.seed(Item{ID: "9", Title: "wide change", UpdatedAt: "u1"})
	oldTracker := trackerFor
	trackerFor = func(string) Tracker { return tk }
	defer func() { trackerFor = oldTracker }()

	oldRun := runPhase
	runPhase = func(_ string, _ string, _ *Agent, userPrompt, tracePath string) (runStats, error) {
		return runStats{}, &runTimeoutError{elapsed: 3 * time.Minute, lastEvent: "step_finish"}
	}
	defer func() { runPhase = oldRun }()

	cfg := defaultConfig()
	cfg.Repo = "owner/repo"
	cfg.Projection = ProjectionConfig{}

	it := Item{ID: "9", Title: "wide change", UpdatedAt: "u1"}
	out, err := (builderFlow{}).Act(cfg, repo, Subject{
		Key: "item-9", Kind: "item", Revision: "u1", ID: "9", Item: it,
	}, "run-timeout")
	if err == nil {
		t.Fatalf("a runaway run returned no error: %#v", out)
	}
	if !isRunTimeout(err) {
		t.Fatalf("error %v does not wrap a runTimeoutError", err)
	}
	if out.Status != "timeout_failed" {
		t.Fatalf("timeout status = %q, want timeout_failed (mechanical)", out.Status)
	}
	if !strings.Contains(err.Error(), "3m0s") || !strings.Contains(err.Error(), "step_finish") {
		t.Errorf("error %q did not name the elapsed time and last trace event", err)
	}

	subjects, err := (fixerFlow{}).Select(cfg, repo)
	if err != nil {
		t.Fatalf("fixer Select: %v", err)
	}
	if len(subjects) != 0 {
		t.Fatalf("a timeout failure was offered to the Fixer: %#v", subjects)
	}
	if n, err := readAttempts(repo, "branch-forest/9-wide-change"); err != nil || n != 0 {
		t.Fatalf("fixer attempts = (%d, %v), want 0; a timeout must not spend a repair attempt", n, err)
	}
}

func TestRunFlowLoopReloadsConfigBeforeNextPass(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	writeConfig := func(autoMerge bool, message string) {
		t.Helper()
		body := "repo: " + admissionTestRepo + "\n" +
			"checks:\n  - name: test\n    run: \"true\"\n" +
			"flows:\n  verifier:\n    auto_merge: " + strconv.FormatBool(autoMerge) + "\n"
		if err := os.WriteFile(configPath(repo), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitTest(t, repo, "add", "forest.yaml")
		runGitTest(t, repo, "commit", "-m", message)
	}
	writeConfig(false, "initial config")
	cfg, err := loadConfig(configPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	var drain int32
	flow := &reloadConfigFlow{
		observed: make(chan bool, 2),
		release:  make(chan struct{}, 1),
		drain:    &drain,
	}
	done := make(chan struct{})
	go func() {
		runFlowLoop(flow, cfg, repo, &drain, nil)
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case flow.release <- struct{}{}:
		default:
		}
	})
	select {
	case first := <-flow.observed:
		if first {
			t.Fatal("first pass observed auto_merge=true")
		}

	case <-time.After(5 * time.Second):
		t.Fatal("first Flow pass did not start")
	}
	writeConfig(true, "reload config")
	flow.release <- struct{}{}
	select {
	case second := <-flow.observed:
		if !second {
			t.Fatal("next pass did not observe the committed config edit")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Flow pass did not start")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Flow loop did not stop after the reload proof")
	}
}
func TestRunFlowLoopPacesAndRotatesPendingSubjects(t *testing.T) {
	_, repo, _ := notesTestRepository(t)
	body := "repo: " + admissionTestRepo + "\n" +
		"checks:\n  - name: test\n    run: \"true\"\n"
	if err := os.WriteFile(configPath(repo), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, repo, "add", "forest.yaml")
	runGitTest(t, repo, "commit", "-m", "test config")
	cfg, err := loadConfig(configPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	flow := &pendingLoopFlow{
		acted:    make(chan string, 4),
		interval: 150 * time.Millisecond,
	}
	drainNow := make(chan struct{})
	var drain int32
	done := make(chan struct{})
	go func() {
		runFlowLoop(flow, cfg, repo, &drain, drainNow)
		close(done)
	}()
	closed := false
	defer func() {
		if !closed {
			close(drainNow)
		}
	}()

	select {
	case key := <-flow.acted:
		if key != "pending-a" {
			t.Fatalf("first pending Subject = %q, want pending-a", key)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first pending Subject did not act")
	}
	select {
	case key := <-flow.acted:
		t.Fatalf("pending loop acted %q before its interval", key)
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case key := <-flow.acted:
		if key != "pending-b" {
			t.Fatalf("second pending Subject = %q, want pending-b", key)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second pending Subject did not act after its interval")
	}
	close(drainNow)
	closed = true
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("pending loop did not stop")
	}
}

func TestUpdateGateWaitsForEveryActiveFlow(t *testing.T) {
	repo, _ := newAdmissionRepositories(t)
	first := Subject{Key: "item-update-a", Kind: "item", ID: "update-a", Revision: "r1"}
	second := Subject{Key: "item-update-b", Kind: "item", ID: "update-b", Revision: "r1"}
	flow := gateHoldingFlow{
		ready: make(chan string, 2),
		releases: map[string]chan struct{}{
			first.Key:  make(chan struct{}, 1),
			second.Key: make(chan struct{}, 1),
		},
	}
	t.Cleanup(func() {
		for _, release := range flow.releases {
			select {
			case release <- struct{}{}:
			default:
			}
		}
	})
	results := make(chan struct {
		key  string
		code int
	}, 2)
	for _, subject := range []Subject{first, second} {
		go func(s Subject) {
			results <- struct {
				key  string
				code int
			}{s.Key, actOnSubject(flow, Config{Repo: admissionTestRepo}, repo, s, nil)}
		}(subject)
	}
	for range 2 {
		select {
		case <-flow.ready:
		case <-time.After(5 * time.Second):
			t.Fatal("Flow Effect did not acquire the update read gate")
		}
	}
	updateAcquired := make(chan struct{})
	ticks := make(chan time.Time, 1)
	oldTicker, oldCheck, oldFactoryDir := selfUpdateTicker, selfUpdateCheck, factoryDir
	t.Cleanup(func() {
		selfUpdateTicker, selfUpdateCheck, factoryDir = oldTicker, oldCheck, oldFactoryDir
	})
	selfUpdateTicker = func() (<-chan time.Time, func()) { return ticks, func() {} }
	selfUpdateCheck = func(string, *int32) { close(updateAcquired) }
	factoryDir = repo
	var drain int32
	updateDone := make(chan struct{})
	go func() {
		selfUpdateLoop(repo, &drain, nil)
		close(updateDone)
	}()
	ticks <- time.Now()
	select {
	case <-updateAcquired:
		t.Fatal("source update entered while two Flow Effects were active")
	case <-time.After(100 * time.Millisecond):
	}
	flow.releases[first.Key] <- struct{}{}
	select {
	case result := <-results:
		if result.key != first.Key || result.code != 0 {
			t.Fatalf("first released result = %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first Flow Effect did not finish")
	}
	select {
	case <-updateAcquired:
		t.Fatal("source update entered while the second Flow Effect was active")
	case <-time.After(100 * time.Millisecond):
	}
	flow.releases[second.Key] <- struct{}{}
	select {
	case <-updateAcquired:
	case <-time.After(5 * time.Second):
		t.Fatal("source update did not enter after every Flow Effect finished")
	}
	select {
	case result := <-results:
		if result.key != second.Key || result.code != 0 {
			t.Fatalf("second released result = %#v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Flow Effect did not finish")
	}
	close(ticks)
	select {
	case <-updateDone:
	case <-time.After(5 * time.Second):
		t.Fatal("source update loop did not stop")
	}
}

func TestLoadLedgerRejectsMalformedRowWithLineNumber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := os.WriteFile(path, []byte(`{"flow":"builder"}`+"\n"+`{malformed}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rows, invalid, err := loadLedger(path)
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("malformed Ledger error = %v, want line 2", err)
	}
	if rows != nil || invalid != 0 {
		t.Fatalf("malformed Ledger returned rows=%#v invalid=%d, want no totals", rows, invalid)
	}
}
