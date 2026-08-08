package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"

	"github.com/misty-step/iron-forest/core"
)

// surfaceFiles are the read-command surfaces that must reach state only through
// the core API. They are distinct from the flow files, which legitimately run
// git and gh as part of a live run.
var surfaceFiles = []string{"main.go", "stats.go", "watch.go"}

// stateHelpers are the package functions the surfaces used to read state
// directly before #176 routed them through the core API.
var stateHelpers = map[string]bool{
	"loadLedger":     true,
	"fetchNotes":     true,
	"readVerdict":    true,
	"readChecks":     true,
	"discoverAgents": true,
	"loadAgent":      true,
	"gitOut":         true,
	"gitOutRaw":      true,
	"probeDaemon":    true,
	"worktreePaths":  true,
	"eligibleItems":  true,
}

// TestSurfacesReachStateOnlyThroughCore guards the #176 seam: the five read
// commands must obtain every piece of state from the core API and never fall
// back to calling a state helper directly. It walks the surface files' call
// expressions and fails on any direct helper call, so a future edit that wires
// a surface straight to the ledger, notes, git, or tracker is caught here.
func TestSurfacesReachStateOnlyThroughCore(t *testing.T) {
	for _, name := range surfaceFiles {
		fset := token.NewFileSet()
		node, err := parser.ParseFile(fset, name, nil, parser.AllErrors)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		var hits []string
		ast.Inspect(node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if stateHelpers[id.Name] {
				hits = append(hits, id.Name)
			}
			return true
		})
		if len(hits) > 0 {
			t.Errorf("%s calls state helper(s) directly (%s); route through the core API instead", name, strings.Join(hits, ", "))
		}
	}
}

// The build/define-time guards for names the surfaces used but that are not
// ordinary function calls in the AST (a Flow selector). They must not appear in
// a surface file at all.
func TestSurfacesDoNotMentionFlowSelector(t *testing.T) {
	for _, name := range surfaceFiles {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.Contains(string(b), "builderFlow") {
			t.Errorf("%s references the builder Flow selector; route list through the core API instead", name)
		}
	}
}

// captureOutput runs fn with os.Stdout and os.Stderr redirected to pipes and
// returns whatever the function wrote to each.
func captureOutput(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, _ := os.Pipe()
	rErr, wErr, _ := os.Pipe()
	os.Stdout, os.Stderr = wOut, wErr
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()

	done := make(chan struct{})
	var outBuf, errBuf bytes.Buffer
	go func() {
		_, _ = io.Copy(&outBuf, rOut)
		_, _ = io.Copy(&errBuf, rErr)
		close(done)
	}()
	fn()
	_ = wOut.Close()
	_ = wErr.Close()
	<-done
	return outBuf.String(), errBuf.String()
}

// TestCoreEligibleKeepsStalledWhileItemsFilters pins the #176 selection
// semantics: `watch --live-gh` must keep showing the raw eligible backlog even
// after an item hits the builder's stall brake, because that brake is what
// `forest list` narrows away.
func TestCoreEligibleKeepsStalledWhileItemsFilters(t *testing.T) {
	old := trackerFor
	trackerFor = func(repo string) Tracker {
		return trackerStub{items: []Item{
			{ID: "hab_01J9X", Title: "opaque", UpdatedAt: "r"},
		}}
	}
	defer func() { trackerFor = old }()

	api, work, _ := coreFixture(t)
	for range stalledRunLimit {
		if err := recordStalled(work, "builder", "item-hab_01J9X", "r"); err != nil {
			t.Fatalf("recordStalled: %v", err)
		}
	}

	items, err := api.Items()
	if err != nil {
		t.Fatalf("Items: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("Items = %+v, want the stalled item filtered out of the list backlog", items)
	}

	eligible, err := api.EligibleItems()
	if err != nil {
		t.Fatalf("EligibleItems: %v", err)
	}
	if len(eligible) != 1 || eligible[0].ID != "hab_01J9X" {
		t.Fatalf("EligibleItems = %+v, want the stalled item kept for the live board", eligible)
	}
}

// TestCmdAgentsReportsMalformedAndContinues pins the #176 agents behavior: one
// malformed declaration is reported on stderr but the rest still list, instead
// of the first error aborting the whole command.
func TestCmdAgentsReportsMalformedAndContinues(t *testing.T) {
	api, work, _ := coreFixture(t)
	good := filepath.Join(work, DefaultAgentsDir, "good")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "agent.yaml"), []byte("description: good\nmodel: g-model\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "instructions.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(work, DefaultAgentsDir, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	// No model: loadAgent refuses the declaration, but the command must not stop.
	if err := os.WriteFile(filepath.Join(bad, "agent.yaml"), []byte("description: broken\nmode: primary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "instructions.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code := 0
	stdout, stderr := captureOutput(t, func() { code = cmdAgents(api) })
	if code != 0 {
		t.Fatalf("cmdAgents code = %d, want 0", code)
	}
	if !strings.Contains(stderr, "forest: agent bad: model is required") {
		t.Errorf("stderr missing malformed-agent report:\n%s", stderr)
	}
	if !strings.Contains(stdout, "good\tmodel=g-model") {
		t.Errorf("stdout missing the good agent listing after a malformed one:\n%s", stdout)
	}
	if strings.Contains(stdout, "bad\tmodel=") {
		t.Errorf("stdout must not list the malformed agent:\n%s", stdout)
	}
}

// stubAPI is a minimal core.API for command tests that exercise one method.
type stubAPI struct {
	notesFn func(sha string) (core.Verdict, core.Checks, error)
}

func (stubAPI) Config() (core.Config, error)                           { return core.Config{}, nil }
func (stubAPI) Agents() ([]core.AgentInfo, error)                      { return nil, nil }
func (stubAPI) Ledger(core.LedgerQuery) ([]core.RunRecord, int, error) { return nil, 0, nil }
func (stubAPI) Trace(string) ([]byte, error)                           { return nil, nil }
func (s stubAPI) Notes(sha string) (core.Verdict, core.Checks, error)  { return s.notesFn(sha) }
func (stubAPI) Items() ([]core.Item, error)                            { return nil, nil }
func (stubAPI) EligibleItems() ([]core.Item, error)                    { return nil, nil }
func (stubAPI) Branches() ([]core.BranchState, error)                  { return nil, nil }
func (stubAPI) Head() (string, error)                                  { return "", nil }
func (stubAPI) Worktrees() ([]string, error)                           { return nil, nil }
func (stubAPI) Daemon() (core.Daemon, error)                           { return core.Daemon{}, nil }

// TestCmdShowRestoresNoteErrorPrefixes pins the #176 show behavior: a failure
// in each note subsystem keeps the per-subsystem stderr prefix the command has
// always printed.
func TestCmdShowRestoresNoteErrorPrefixes(t *testing.T) {
	cases := []struct {
		stage core.ErrorStage
		want  string
	}{
		{core.StageFetch, "forest: notes:"},
		{core.StageVerdict, "forest: verdict:"},
		{core.StageChecks, "forest: checks:"},
	}
	for _, tc := range cases {
		api := stubAPI{notesFn: func(string) (core.Verdict, core.Checks, error) {
			return core.Verdict{}, core.Checks{}, &core.StageError{Stage: tc.stage, Err: errors.New("boom")}
		}}
		code := 0
		_, stderr := captureOutput(t, func() { code = cmdShow(api, "abc") })
		if code != 1 {
			t.Fatalf("stage %s: cmdShow code = %d, want 1", tc.stage, code)
		}
		if want := tc.want + " boom"; !strings.Contains(stderr, want) {
			t.Errorf("stage %s: stderr %q, want it to contain %q", tc.stage, stderr, want)
		}
	}
}

// TestCmdShowPreservesResultsNilVersusEmptyShape pins the #176 show
// byte-for-byte behavior against the pre-#176 surface output: a checks note
// whose results field is absent or null (e.g. writeChecks with only a status)
// renders `results: null` exactly as it did before, and an explicit empty array
// renders `results: []`. The nil-versus-empty shape is preserved through the
// API; it is never collapsed one way or the other.
func TestCmdShowPreservesResultsNilVersusEmptyShape(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"null", `"results": null`},
		{"explicit-empty", `"results": []`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api, work, sha := coreFixture(t)
			if tc.name == "explicit-empty" {
				if err := writeChecks(work, sha, checksNote{Status: "pass", Results: []checkResult{}}); err != nil {
					t.Fatal(err)
				}
			} else {
				// A checks note carrying only a status has a null (nil) results
				// field once the note is written and read back, so it exercises
				// the same path as an explicit JSON null.
				if err := writeChecks(work, sha, checksNote{Status: "pass"}); err != nil {
					t.Fatal(err)
				}
			}
			code := 0
			out, _ := captureOutput(t, func() { code = cmdShow(api, sha) })
			if code != 0 {
				t.Fatalf("cmdShow code = %d, want 0", code)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output missing %s:\n%s", tc.want, out)
			}
		})
	}
}

// TestCmdShowEmitsNoteLackingTimeAndRunID pins the pre-#176 show behavior that
// cmdShow kept when reaching state through the core API: presence follows the
// note's existence, not a field heuristic. A valid verdict that carries neither
// a time nor a run id is still a present decision and must be emitted.
func TestCmdShowEmitsNoteLackingTimeAndRunID(t *testing.T) {
	api, work, sha := coreFixture(t)
	// writeNote bypasses writeVerdict's time stamp, leaving both Time and RunID
	// empty in the stored note.
	if err := writeNote(work, verdictNotesRef, sha, verdictNote{Verdict: "approve"}); err != nil {
		t.Fatal(err)
	}
	code := 0
	out, _ := captureOutput(t, func() { code = cmdShow(api, sha) })
	if code != 0 {
		t.Fatalf("cmdShow code = %d, want 0", code)
	}
	if !strings.Contains(out, `"verdict": "approve"`) {
		t.Errorf("output = %q, want the verdict note emitted despite missing time and run id", strings.TrimSpace(out))
	}
}
