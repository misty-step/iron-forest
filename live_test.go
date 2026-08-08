package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// cancellableFlow is a Flow stub whose Act blocks inside runPhase until the run
// is cancelled, exactly like a real lane that is mid-run. Tests drive
// actOnSubject with it to exercise the live registry and socket end to end
// without a real tracker or opencode.
type cancellableFlow struct{}

func (cancellableFlow) Name() string                             { return "builder" }
func (cancellableFlow) Select(Config, string) ([]Subject, error) { return aSubject, nil }
func (cancellableFlow) Interval(Config) time.Duration            { return 0 }
func (cancellableFlow) Enabled(Config) bool                      { return true }
func (cancellableFlow) Act(_ Config, repoDir string, _ Subject, runID string) (Outcome, error) {
	stats, err := runPhase(repoDir, "", nil, "task", "", runID)
	return Outcome{TokIn: stats.tokensIn}, err
}

// TestLiveStatusAndCancel drives the daemon's live pathway end to end: start a
// fake in-flight run, query status over the socket, cancel it over the socket,
// and assert the recorded row names the cancellation and its reason separate
// from agent_failed, with the spend kept.
func TestLiveStatusAndCancel(t *testing.T) {
	repo := t.TempDir()
	workspace := workspaceDir(repo)
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}

	oldRun := runPhase
	runPhase = func(_ string, _ string, _ *Agent, _ string, _ string, id string) (runStats, error) {
		ctx, cancel := context.WithCancel(context.Background())
		liveTrack.attach(id, "canceller", cancel)
		<-ctx.Done()
		return runStats{tokensIn: 7}, &runCancelledError{reason: liveTrack.reason(id)}
	}
	defer func() { runPhase = oldRun }()

	var drain int32
	l, err := liveServerStart(repo, &drain)
	if err != nil {
		t.Fatalf("liveServerStart: %v", err)
	}
	defer liveServerStop(repo, l)

	done := make(chan int, 1)
	go func() {
		done <- actOnSubject(cancellableFlow{}, Config{}, repo, aSubject[0], nil)
	}()

	// The run registers as soon as actOnSubject claims it; wait for it to be
	// visible to a status query so the assertion is deterministic.
	var got []liveRunView
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		got = liveTrack.snapshot()
		if len(got) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 1 {
		t.Fatalf("status saw %d runs, want 1", len(got))
	}
	if got[0].Flow != "builder" || got[0].Subject != "item-1" ||
		got[0].Revision != "rev-1" || got[0].Agent != "canceller" || got[0].RunID == "" {
		t.Fatalf("status run = %+v, want the claimed fields", got[0])
	}

	resp, err := liveClient(repo, liveRequest{Type: "cancel", RunID: got[0].RunID, By: "operator", Reason: "testing cancel"})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if !resp.OK {
		t.Fatalf("cancel response not ok: %+v", resp)
	}

	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("cancelled run code = %d, want 1", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled run did not stop")
	}

	rows, _, err := loadLedger(ledgerPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1]
	if last.Status != "cancelled" {
		t.Errorf("cancelled status = %q, want %q (not agent_failed)", last.Status, "cancelled")
	}
	if !strings.Contains(last.Error, "testing cancel") {
		t.Errorf("cancelled reason %q does not name the request reason", last.Error)
	}
	if last.TokensIn != 7 {
		t.Errorf("cancelled run tokensIn = %d, want 7 (spend kept)", last.TokensIn)
	}
}

// TestCancelAcceptedBeforeAttachIsHonored pins the atomicity of the pending
// cancel and the cancel handle: a request accepted while the run is visible but
// before attach stored its context must still stop the run. Before the fix, the
// later attach never honored the pending request, so a successful cancel
// response let the run continue (see #163).
func TestCancelAcceptedBeforeAttachIsHonored(t *testing.T) {
	id := "20260808T000000Z-item-1"
	cancelled := make(chan struct{})
	lr := &liveRun{id: id, started: time.Now()}
	liveTrack.begin(lr)

	// Accept the cancel while the run has no cancel handle yet.
	if !liveTrack.cancel(id, "early") {
		t.Fatalf("cancel did not find the run %s", id)
	}
	// The run now attaches its handle; the pending request must fire it.
	var once sync.Once
	liveTrack.attach(id, "a", func() {
		once.Do(func() { close(cancelled) })
	})
	select {
	case <-cancelled:
	case <-time.After(1 * time.Second):
		t.Fatal("a cancel accepted before attach did not stop the run")
	}
	liveTrack.end(id)
}

// TestHoistLiveFlags pins that the advertised `forest cancel <run-id> --reason
// ...` form is parsed: Go's FlagSet stops at the first positional token, so a
// reason flag written after the run id must be hoisted ahead of it (see #163).
func TestHoistLiveFlags(t *testing.T) {
	out := hoistLiveFlags([]string{"run-1", "--reason", "blue", "--by", "bob"})
	want := []string{"--reason", "blue", "--by", "bob", "run-1"}
	if len(out) != len(want) {
		t.Fatalf("hoistLiveFlags = %v, want %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("hoistLiveFlags = %v, want %v", out, want)
		}
	}
	// Equals-form flags and a positional-only call keep working too.
	out = hoistLiveFlags([]string{"--reason=why", "run-2"})
	if len(out) != 2 || out[0] != "--reason=why" || out[1] != "run-2" {
		t.Fatalf("hoistLiveFlags = %v, want [--reason=why run-2]", out)
	}
	if got := hoistLiveFlags([]string{"run-3"}); len(got) != 1 || got[0] != "run-3" {
		t.Fatalf("hoistLiveFlags = %v, want [run-3]", got)
	}
}

// TestLiveRequestWithNoDaemonErrors pins the acceptance that a live request
// with no daemon running returns a named error and never reads the ledger.
func TestLiveRequestWithNoDaemonErrors(t *testing.T) {
	repo := t.TempDir()
	resp, err := liveClient(repo, liveRequest{Type: "status"})
	if err == nil {
		t.Fatalf("live status with no daemon returned ok: %+v", resp)
	}
	if !strings.Contains(err.Error(), "no live daemon") {
		t.Errorf("error %q must name that no daemon is running", err)
	}
}

// TestLiveSocketRemovedOnShutdownAndStaleNotBlocking pins the two socket
// lifecycle guarantees: liveServerStop removes the socket, and a stale socket
// left by a crashed daemon does not block startup.
func TestLiveSocketRemovedOnShutdownAndStaleNotBlocking(t *testing.T) {
	repo := t.TempDir()
	path := liveSocketPath(repo)

	// A stale socket from a crashed process must not block a fresh daemon.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	stale, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = stale.Close()

	var drain int32
	l, err := liveServerStart(repo, &drain)
	if err != nil {
		t.Fatalf("liveServerStart over a stale socket: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("fresh socket not listening at %s: %v", path, err)
	}

	liveServerStop(repo, l)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket %s still exists after clean shutdown", path)
	}
}
