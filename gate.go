package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// report is the typed envelope the build agent must write before it stops.
// It is the only channel that crosses the seam between agent and factory.
type report struct {
	Summary      string   `json:"summary"`
	ChangedFiles []string `json:"changed_files"`
	Notes        string   `json:"notes"`
}

// review is the verdict the review agent must write.
type review struct {
	Verdict string `json:"verdict"` // approve | changes
	Summary string `json:"summary"`
	Notes   string `json:"notes"`
}

// runArtifacts are the per-run ledger files the agent is allowed to write; the
// gate and publish step treat them as records, not repository changes.
var runArtifacts = []string{"report.json", "review.json"}

// protectedPaths are the factory's own control plane. A build run must not
// change them, because a run that rewrites the daemon's configuration, its
// agent declarations, its orchestration state, or its harness wiring could
// rewrite the very rules, prompts, permissions, and budget it runs under. These
// are the paths that keep a delivery machine honest, so the Gate refuses any
// run whose change touches one -- independent of anything the report claims.
var protectedPaths = []string{
	".forest/",
	"forest.yaml",
	"agents/",
	".opencode/opencode.json",
}

// isProtectedPath reports whether a changed path sits in the factory's control
// plane and is therefore off-limits to a run.
func isProtectedPath(path string) bool {
	for _, p := range protectedPaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// gateRejectedPaths scans git status --porcelain and returns an error if any
// changed path -- including the source of a rename -- sits in the factory's
// control plane. A rename is inspected on both sides: a staged rename that
// moves a protected file out of agents/ or another protected path is still a
// change to a protected path, so the Gate refuses it even though the
// destination alone is outside the control plane.
func gateRejectedPaths(porcelain string) error {
	for _, line := range strings.Split(porcelain, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		source := path
		if i := strings.Index(path, " -> "); i >= 0 {
			source = path[:i]
			path = path[i+4:]
		}
		offender := ""
		if isProtectedPath(source) {
			offender = source
		} else if isProtectedPath(path) {
			offender = path
		}
		if offender != "" {
			return fmt.Errorf("change touches protected path %q", offender)
		}
	}
	return nil
}

// gateIgnoredProtected scans the output of `git status --porcelain --ignored`
// and returns an error if any ignored path sits in the factory's control plane.
// The plain porcelain never lists an ignored path: /.forest/ is git-ignored, so
// a run that changes .forest/foo alongside an ordinary source file would hide
// the control-plane mutation and pass the Gate. Because the plain porcelain
// cannot see it, an ignored protected path is checked on its own, from the set
// of paths git reports as ignored.
func gateIgnoredProtected(porcelain string) error {
	for _, line := range strings.Split(porcelain, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		if isProtectedPath(path) {
			return fmt.Errorf("change touches protected path %q", path)
		}
	}
	return nil
}

// gate verifies the build agent's claims against reality after the run:
//   - the agent did not commit (HEAD is still the base)
//   - it did not touch a protected path
//   - it produced a non-empty change
//   - report.json exists and satisfies its declared schema
//
// It returns the changed file list that becomes the pull request body.
func gate(wtDir, baseSHA, schemaPath string) ([]string, report, error) {
	var rep report
	head, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		return nil, rep, fmt.Errorf("cannot read worktree HEAD: %w", err)
	}
	if head != baseSHA {
		return nil, rep, fmt.Errorf("agent committed: HEAD moved %s -> %s", short(baseSHA), short(head))
	}
	out, err := gitOutRaw(wtDir, "status", "--porcelain")
	if err != nil {
		return nil, rep, err
	}
	// The protected-path check runs over the raw porcelain so a staged rename
	// is inspected on both sides, not just its destination.
	if err := gateRejectedPaths(out); err != nil {
		return nil, rep, err
	}
	// A protected path that is git-ignored never appears in the porcelain above:
	// /.forest/ is git-ignored, so a run mutating .forest/foo alongside ordinary
	// work must not hide the control-plane change. Ask git for the ignored set
	// and refuse if any ignored path is protected.
	ign, err := gitOutRaw(wtDir, "status", "--porcelain", "--ignored", "--untracked-files=all")
	if err != nil {
		return nil, rep, err
	}
	if err := gateIgnoredProtected(ign); err != nil {
		return nil, rep, err
	}
	changed := parseChanged(out)
	real := make([]string, 0, len(changed))
	for _, path := range changed {
		if isRunArtifact(path) {
			continue // a build's own report, not the repo's change
		}
		real = append(real, path)
	}
	if len(real) == 0 {
		return nil, rep, fmt.Errorf("agent produced no real changes")
	}
	repFile := filepath.Join(wtDir, "report.json")
	if err := checkSchema(repFile, schemaPath); err != nil {
		return nil, rep, err
	}
	raw, err := os.ReadFile(repFile)
	if err != nil {
		return nil, rep, fmt.Errorf("report.json missing: %w", err)
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, rep, fmt.Errorf("report.json is invalid JSON: %w", err)
	}
	return real, rep, nil
}

// gateReview parses the review agent's review.json and validates its verdict.
func gateReview(wtDir, schemaPath string) (review, error) {
	rvFile := filepath.Join(wtDir, "review.json")
	var rv review
	raw, err := os.ReadFile(rvFile)
	if err != nil {
		return rv, fmt.Errorf("review.json missing: %w", err)
	}
	if err := checkSchema(rvFile, schemaPath); err != nil {
		return rv, err
	}
	if err := json.Unmarshal(raw, &rv); err != nil {
		return rv, fmt.Errorf("review.json is invalid JSON: %w", err)
	}
	if rv.Verdict != "approve" && rv.Verdict != "changes" {
		return rv, fmt.Errorf("review.verdict must be approve or changes, got %q", rv.Verdict)
	}
	return rv, nil
}

// checkSchema validates a JSON run artifact against its declared JSON Schema:
// the file parses, and every required property is present and non-empty.
func checkSchema(file, schemaPath string) error {
	sb, err := os.ReadFile(schemaPath)
	if err != nil {
		return nil // no declared schema; rely on the typed struct
	}
	var s struct {
		Required   []string `json:"required"`
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(sb, &s); err != nil {
		return fmt.Errorf("schema %s: %w", schemaPath, err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	for _, name := range s.Required {
		raw, ok := v[name]
		if !ok {
			return fmt.Errorf("%s missing required field %q", filepath.Base(file), name)
		}
		switch s.Properties[name].Type {
		case "string":
			var sval string
			if err := json.Unmarshal(raw, &sval); err != nil {
				return err
			}
			if strings.TrimSpace(sval) == "" {
				return fmt.Errorf("%s field %q is empty", filepath.Base(file), name)
			}
		case "array":
			var arr []json.RawMessage
			if err := json.Unmarshal(raw, &arr); err != nil {
				return err
			}
			if len(arr) == 0 {
				return fmt.Errorf("%s field %q is empty", filepath.Base(file), name)
			}
		}
	}
	return nil
}

func isRunArtifact(path string) bool {
	for _, a := range runArtifacts {
		if path == a {
			return true
		}
	}
	return false
}

// parseChanged turns `git status --porcelain` into a changed file list,
// stripping rename arrows (`R  old -> new` keeps `new`).
func parseChanged(porcelain string) []string {
	var out []string
	for _, line := range strings.Split(porcelain, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 4 {
			continue
		}
		path := line[3:]
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		out = append(out, path)
	}
	return out
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
