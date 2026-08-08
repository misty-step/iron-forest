package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// gate verifies the build agent's claims against reality after the run:
//   - the agent did not commit (HEAD is still the base)
//   - it produced a non-empty change
//   - report.json exists, satisfies its declared schema, and its changed_files
//     cross-check against the real change
//
// It returns the changed file list that becomes the pull request body.
//
// There is no protected-path check. See docs/adr/0003: the list was not a
// security boundary, because the code enforcing it was itself writable by any
// run, and it blocked the factory from working on its own declarations. The
// boundary that holds is independent review on the exact commit.
func gate(wtDir, baseSHA, schemaPath, tracePath string) ([]string, report, error) {
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
	changed := parseChanged(out)
	real := make([]string, 0, len(changed))
	for _, path := range changed {
		if strings.HasPrefix(path, ".forest/") || isRunArtifact(path) {
			continue // a run record, not the repo's change
		}
		real = append(real, path)
	}
	if len(real) == 0 {
		return nil, rep, fmt.Errorf("agent produced no real changes")
	}
	repFile := filepath.Join(wtDir, "report.json")
	if err := checkSchema(repFile, schemaPath); err != nil {
		return nil, rep, reportMissingTrace(err, tracePath)
	}
	raw, err := os.ReadFile(repFile)
	if err != nil {
		return nil, rep, reportMissingTrace(err, tracePath)
	}
	if err := json.Unmarshal(raw, &rep); err != nil {
		return nil, rep, fmt.Errorf("report.json is invalid JSON: %w", err)
	}
	if err := crossCheck(real, rep.ChangedFiles); err != nil {
		return nil, rep, err
	}
	return real, rep, nil
}

// crossCheck refuses a report whose changed_files misdescribes the real change:
// it names a claimed file that did not change, and a changed file the report
// omits. Paths are normalised on both sides so a rename is judged on the path
// it now names rather than on an accidental slash or dot difference.
func crossCheck(real, claimed []string) error {
	realSet := make(map[string]bool, len(real))
	for _, p := range real {
		realSet[normalizePath(p)] = true
	}
	claimedSet := make(map[string]bool, len(claimed))
	for _, p := range claimed {
		claimedSet[normalizePath(p)] = true
	}
	for _, p := range claimed {
		if !realSet[normalizePath(p)] {
			return fmt.Errorf("report claims changed file %q that did not change", p)
		}
	}
	for _, p := range real {
		if !claimedSet[normalizePath(p)] {
			return fmt.Errorf("report omits changed file %q", p)
		}
	}
	return nil
}

// normalizePath reduces a porcelain or reported path to a comparable form:
// forward slashes, cleaned of "." and "..", no leading separators.
func normalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return strings.Trim(filepath.ToSlash(filepath.Clean(p)), "/")
}

// reportMissingTrace augments an error reading report.json with the trace tail
// so an operator sees where a run stopped instead of only "report.json
// missing". It augments only an absent file; any other read or schema error is
// returned unchanged.
func reportMissingTrace(err error, tracePath string) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("report.json missing: %w\ntrace tail:\n%s", err, traceTail(tracePath, 5))
	}
	return err
}

// traceTail returns up to n trailing non-empty lines of a trace file, each
// capped by traceEventLabel, so a failure names where the run stopped. A
// missing, unreadable, or empty trace reports "(no trace available)".
func traceTail(path string, n int) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(no trace available)"
	}
	lines := strings.Split(string(b), "\n")
	var tail []string
	for len(lines) > 0 && len(tail) < n {
		line := strings.TrimSpace(lines[len(lines)-1])
		lines = lines[:len(lines)-1]
		if line == "" {
			continue
		}
		tail = append([]string{traceEventLabel(line)}, tail...)
	}
	if len(tail) == 0 {
		return "(no trace available)"
	}
	return strings.Join(tail, "\n")
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

// reviewTreeSnapshot is the complete tracked state of a review worktree at one
// moment: for every path the index tracks, the working-tree content hash and the
// blob the index holds for it. Two snapshots of the same revision are equal
// exactly when no tracked file's content or index state moved in between, so a
// full comparison can prove a review left the tree untouched.
type reviewTreeSnapshot struct {
	work map[string]string // path -> sha1 of the working-tree bytes; "" when gone
	idx  map[string]string // path -> index blob sha; absent when not in the index
}

// assertCleanReviewTree refuses a Verdict from a review run that did not leave
// the tracked worktree unchanged. A Verifier may write review.json — the one
// file it is expected to produce — but any tracked-file change between the
// before-snapshot and the run's end, or any move of HEAD, means the agent edited
// the tree the run was meant to judge: the checks that back a Verdict would then
// describe an uncommitted experiment, never the committed Review revision. before
// must be the snapshot taken immediately before the review runs. The comparison
// is complete and symmetric — it records working-tree content and index state per
// tracked path — so editing a file that was already dirty, staging it, or
// restoring it to clean is refused exactly as a fresh edit is. It returns an
// error naming every offending path when the tree drifted, and nil when only the
// expected review record remains. review.json is untracked, so it never enters a
// snapshot and needs no special case here.
func assertCleanReviewTree(wtDir, head string, before reviewTreeSnapshot) error {
	cur, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("review: cannot read worktree HEAD: %w", err)
	}
	if cur != head {
		return fmt.Errorf("review moved HEAD: reviewed %s -> %s", short(head), short(cur))
	}
	after, err := snapshotReviewTree(wtDir)
	if err != nil {
		return fmt.Errorf("review: %w", err)
	}
	keys := make(map[string]bool, len(before.work)+len(after.work))
	for path := range before.work {
		keys[path] = true
	}
	for path := range after.work {
		keys[path] = true
	}
	var dirty []string
	for _, path := range sortedPaths(keys) {
		if before.work[path] != after.work[path] || before.idx[path] != after.idx[path] {
			dirty = append(dirty, path)
		}
	}
	if len(dirty) > 0 {
		return fmt.Errorf("review left the worktree dirty: %s", strings.Join(dirty, ", "))
	}
	return nil
}

// sortedPaths returns the keys of m in deterministic order so refusals name
// paths consistently instead of in map-iteration order.
func sortedPaths(m map[string]bool) []string {
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// snapshotReviewTree captures the complete state of every tracked path: whether
// the working file exists and its content, and the blob the index holds for it.
func snapshotReviewTree(wtDir string) (reviewTreeSnapshot, error) {
	out, err := gitOutRaw(wtDir, "ls-files", "-s")
	if err != nil {
		return reviewTreeSnapshot{}, fmt.Errorf("review: read index: %w", err)
	}
	snap := reviewTreeSnapshot{
		work: make(map[string]string),
		idx:  make(map[string]string),
	}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		// "ls-files -s" emits "<mode> <blob> <stage>\t<path>".
		meta, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 2 {
			continue
		}
		snap.idx[path] = fields[1] // the index blob of the tracked path
		snap.work[path] = workHash(wtDir, path)
	}
	return snap, nil
}

// workHash returns the sha1 of the working-tree bytes of path, or "" when the
// file is absent from the working tree; a missing file means the tracked path
// was deleted, which is as much a change as a rewrite is.
func workHash(wtDir, path string) string {
	data, err := os.ReadFile(filepath.Join(wtDir, path))
	if err != nil {
		return ""
	}
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
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
