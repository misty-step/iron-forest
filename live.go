package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// liveRun is one in-flight agent run the daemon owns and can serve over the
// local socket. id, flow, subject, revision and started are known when the run
// is claimed in actOnSubject; agent and cancel are attached the moment runPhase
// starts the child and builds the cancellable context. cancelReason, once set,
// records a pending cancellation and who requested it and why, so the ledger can
// name the cancel apart from an agent failure (see #163).
type liveRun struct {
	id           string
	flow         string
	subject      string
	revision     string
	agent        string
	started      time.Time
	cancel       context.CancelFunc
	cancelReason string
}

// liveRegistry is the process-global set of in-flight runs. It is the single
// source of truth for "what is running right now" while the daemon lives.
type liveRegistry struct {
	mu   sync.Mutex
	runs map[string]*liveRun
}

var liveTrack = &liveRegistry{runs: make(map[string]*liveRun)}

// begin records a newly claimed run before its agent has started.
func (r *liveRegistry) begin(lr *liveRun) {
	r.mu.Lock()
	r.runs[lr.id] = lr
	r.mu.Unlock()
}

// attach fills a run's agent and cancel handle the moment runPhase starts the
// child. A run that never reaches runPhase keeps the zero values, which is a
// truthful "claimed but not yet started". If a cancel was already accepted while
// the run was visible but before this handle was stored, the pending request is
// honored immediately so the run never outlives an accepted cancellation: a
// cancel call and this attach serialize on the same lock, so the pending
// cancelReason is always visible here (see #163).
func (r *liveRegistry) attach(id, agent string, cancel context.CancelFunc) {
	r.mu.Lock()
	if lr, ok := r.runs[id]; ok {
		lr.agent = agent
		lr.cancel = cancel
		if cancel != nil && lr.cancelReason != "" {
			lr.cancel()
		}
	}
	r.mu.Unlock()
}

// end removes a run that finished from the registry. It is the tear-down used
// by tests; production finalization goes through finish or
// decideWorktreePreserved, both of which remove the run atomically with reading
// its cancel state (see #163).
func (r *liveRegistry) end(id string) {
	r.mu.Lock()
	delete(r.runs, id)
	r.mu.Unlock()
}

// finish removes a run from the registry and returns the cancellation reason
// recorded before this instant, or "" when the run was not cancelled. It is the
// single handoff between the live socket and the ledger append: the removal and
// the reason read happen in one critical section, so a cancel accepted before
// this call is reflected in the returned reason and one arriving after it is
// refused with "no live run". The ledger row's status therefore can never be a
// success written after a cancel the socket already confirmed, and the flow
// cleanup (decideWorktreePreserved) already settled the worktree's fate under
// the same rule (see #163).
func (r *liveRegistry) finish(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	lr, ok := r.runs[id]
	if !ok {
		return ""
	}
	delete(r.runs, id)
	return lr.cancelReason
}

// decideWorktreePreserved reports whether a run's worktree must be kept for
// inspection because a cancel was accepted. It is the worktree's handoff with
// the live socket: the decision and the registry removal happen in one critical
// section, so a cancel accepted before this call is seen here (and the run stays
// in the registry for finish to name on the ledger), while one accepted after it
// is refused outright — the worktree removal that follows can never contradict a
// cancel the socket already confirmed (see #163).
func (r *liveRegistry) decideWorktreePreserved(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	lr, ok := r.runs[id]
	if !ok {
		return false
	}
	if lr.cancelReason != "" {
		return true
	}
	delete(r.runs, id)
	return false
}

// cancel records who requested the cancellation and why, then stops the run's
// context. It reports whether the run existed. Reason joins who and why in one
// auditable string; an empty request records "operator". The cancel handle is
// invoked under the mutex, never after releasing it, so a concurrent attach that
// reads the pending reason under the same lock and a concurrent cancel both
// observe the handle through one serialized access: no unsynchronized read of
// lr.cancel can race a write in attach (see #163).
func (r *liveRegistry) cancel(id, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	lr, ok := r.runs[id]
	if ok {
		lr.cancelReason = reason
		if lr.cancel != nil {
			lr.cancel()
		}
	}
	return ok
}

// reason returns a run's recorded cancellation reason, or the empty string when
// the run never had one (or is gone).
func (r *liveRegistry) reason(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if lr, ok := r.runs[id]; ok {
		return lr.cancelReason
	}
	return ""
}

// cancelGate returns the runCancelledError naming a pending cancel for the run,
// or nil when none is pending. A flow calls it the moment runPhase returns so a
// cancel accepted then halts the run before subsequent checks, pushes, or
// merges can act on a run the socket already cancelled. It is a best-effort
// early abort: the registry's finish is the authoritative decision at record
// time, so a cancel slipping past the gate is still recorded as cancelled (see
// #163).
func cancelGate(runID string) error {
	if reason := liveTrack.reason(runID); reason != "" {
		return &runCancelledError{reason: reason}
	}
	return nil
}

// liveRunView is one in-flight run as served over the socket.
type liveRunView struct {
	RunID    string `json:"run_id"`
	Flow     string `json:"flow"`
	Subject  string `json:"subject"`
	Revision string `json:"revision"`
	Agent    string `json:"agent"`
	Started  string `json:"started"`
}

// snapshot returns every in-flight run's view, sorted by run id.
func (r *liveRegistry) snapshot() []liveRunView {
	r.mu.Lock()
	defer r.mu.Unlock()
	views := make([]liveRunView, 0, len(r.runs))
	for _, lr := range r.runs {
		views = append(views, liveRunView{
			RunID: lr.id, Flow: lr.flow, Subject: lr.subject,
			Revision: lr.revision, Agent: lr.agent,
			Started: lr.started.Format(time.RFC3339),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].RunID < views[j].RunID })
	return views
}

// liveRequest is one client request over the live socket. Type is "status" or
// "cancel".
type liveRequest struct {
	Type   string `json:"type"`
	RunID  string `json:"run_id,omitempty"`
	Reason string `json:"reason,omitempty"`
	By     string `json:"by,omitempty"`
}

// liveResponse is one server answer over the live socket.
type liveResponse struct {
	OK    bool          `json:"ok"`
	Error string        `json:"error,omitempty"`
	Runs  []liveRunView `json:"runs,omitempty"`
}

// liveSocketPath is the Unix socket a client dials to reach the live daemon. A
// Unix socket under .forest/ makes reachability equal to filesystem access to
// the checkout: no port is opened and no network service is exposed.
func liveSocketPath(repoDir string) string {
	return filepath.Join(repoDir, WorkspaceDir, "live.sock")
}

// liveServerStart binds the live socket and serves requests until stopped. A
// stale socket left by a crashed daemon is removed up front, so a fresh one
// never blocks startup. The singleton lock (acquireSingletonLock) already
// guarantees only one daemon, so removing the file is safe: any socket there
// belongs to a process that is no longer holding the lock.
func liveServerStart(repoDir string, drain *int32) (net.Listener, error) {
	dir := filepath.Join(repoDir, WorkspaceDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := liveSocketPath(repoDir)
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("live socket: %w", err)
	}
	_ = os.Chmod(path, 0o600)
	go serveLiveAcceptor(l, drain)
	return l, nil
}

// liveServerStop closes the listener and removes the socket file, which is what
// lets a later start reuse the path. It is the clean-shutdown counterpart to
// liveServerStart.
func liveServerStop(repoDir string, l net.Listener) {
	_ = l.Close()
	_ = os.Remove(liveSocketPath(repoDir))
}

func serveLiveAcceptor(l net.Listener, drain *int32) {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go serveLiveConn(conn, drain)
	}
}

func serveLiveConn(conn net.Conn, drain *int32) {
	defer conn.Close()
	var req liveRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	var resp liveResponse
	switch req.Type {
	case "status":
		resp.OK = true
		resp.Runs = liveTrack.snapshot()
	case "cancel":
		if req.RunID == "" {
			resp.Error = "cancel requires a run id"
			break
		}
		if !liveTrack.cancel(req.RunID, composeCancelReason(req)) {
			resp.Error = fmt.Sprintf("no live run with id %s", req.RunID)
			break
		}
		resp.OK = true
	default:
		resp.Error = fmt.Sprintf("unknown live request %q", req.Type)
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

// composeCancelReason joins who requested a cancel and why into one auditable
// string. A request naming neither records "operator" so the ledger is never
// ambiguous about what acted.
func composeCancelReason(req liveRequest) string {
	by := strings.TrimSpace(req.By)
	why := strings.TrimSpace(req.Reason)
	switch {
	case by == "" && why == "":
		return "operator"
	case by == "":
		return why
	case why == "":
		return by
	default:
		return by + ": " + why
	}
}

// liveClient sends one request to the running daemon and returns its answer. If
// no daemon is listening it returns an error naming that fact, so a client never
// falls back to a stale ledger read.
func liveClient(repoDir string, req liveRequest) (liveResponse, error) {
	conn, err := net.DialTimeout("unix", liveSocketPath(repoDir), 2*time.Second)
	if err != nil {
		return liveResponse{}, fmt.Errorf("no live daemon is running on %s; start it with `forest serve`", liveSocketPath(repoDir))
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return liveResponse{}, fmt.Errorf("live request: %w", err)
	}
	var resp liveResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return liveResponse{}, fmt.Errorf("live reply: %w", err)
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// cmdLiveStatus asks the running daemon what is in flight and prints a line per
// run. It fails with a named error when no daemon is running; it never reads the
// ledger instead.
func cmdLiveStatus(repoDir string, args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "forest: status: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	resp, err := liveClient(repoDir, liveRequest{Type: "status"})
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest: status:", err)
		return 1
	}
	if len(resp.Runs) == 0 {
		fmt.Println("no runs in flight")
		return 0
	}
	for _, r := range resp.Runs {
		fmt.Printf("%s  flow=%s subject=%s revision=%s agent=%s since=%s\n",
			r.RunID, r.Flow, orDash(r.Subject), orDash(r.Revision), orDash(r.Agent), r.Started)
	}
	return 0
}

// cmdLiveCancel asks the running daemon to stop one in-flight run by id. The
// operator may name who and why so the recorded row is auditable; an unnamed
// cancel records "operator".
func cmdLiveCancel(repoDir string, args []string) int {
	fs := flag.NewFlagSet("cancel", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	reason := ""
	by := "operator"
	fs.StringVar(&reason, "reason", "", "why the run is cancelled")
	fs.StringVar(&by, "by", "operator", "who requested the cancel")
	// Go's FlagSet stops parsing at the first non-flag argument, so the
	// advertised `forest cancel <run-id> --reason ...` form would leave the
	// reason flag unparsed and reject it as extra arguments. Hoist every flag
	// (and its value) to the front of the list so the flags parse no matter
	// where they appear relative to the positional run id (see #163).
	if err := fs.Parse(hoistLiveFlags(args)); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "forest: cancel: expected one run id")
		return 2
	}
	_, err := liveClient(repoDir, liveRequest{
		Type: "cancel", RunID: fs.Arg(0), Reason: reason, By: by,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "forest: cancel:", err)
		return 1
	}
	fmt.Printf("cancelling %s\n", fs.Arg(0))
	return 0
}

// hoistLiveFlags reorders an argument list so every flag token (and the value
// of a two-token flag) comes before every positional token. Go's FlagSet stops
// at the first non-flag token, so without this a flag written after a
// positional — `forest cancel <run-id> --reason why` — is never parsed. The
// recognized value-taking flags are reason and by; every other dash-prefixed
// token (such as -h) is left as a flag too, and every other token is positional.
func hoistLiveFlags(args []string) []string {
	var flags, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--reason" || a == "-reason" ||
			a == "--by" || a == "-by":
			flags = append(flags, a)
			if i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		case strings.HasPrefix(a, "--reason=") ||
			strings.HasPrefix(a, "--by="):
			flags = append(flags, a)
		case strings.HasPrefix(a, "-"):
			flags = append(flags, a)
		default:
			rest = append(rest, a)
		}
	}
	return append(flags, rest...)
}
