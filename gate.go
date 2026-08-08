package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// reviewTreeSnapshot is the complete state of a review worktree at one moment:
// the full index entry (mode, blob, stage) of every tracked path, a working-tree
// fingerprint of each tracked leaf, a fingerprint of every parent directory node
// (so a tracked directory swapped for a symlink, or a gitlink's index entry, is
// visible even when the leaves still resolve to identical bytes), the
// fingerprint of every non-ignored untracked path, and, for each gitlink, the
// checked-out submodule's own commit and tracked-file dirtiness. Two snapshots
// of the same revision are equal exactly when no tracked path's type, mode,
// content, index entry or symlink target moved, no parent directory became a
// symlink, no tracked file inside a checked-out submodule changed, and no
// untracked path's type or content changed, so a complete comparison can prove
// a review left the tree untouched. Recording the untracked fingerprint, not a
// bare path set, closes the hole where an existing non-ignored fixture is
// modified in place without changing the set of untracked paths.
type reviewTreeSnapshot struct {
	work      map[string]string // tracked leaf path -> working-tree fingerprint; "" when gone; a gitlink holds its submodule state
	idx       map[string]string // tracked path -> whole "<mode> <blob> <stage>" index entry
	dirs      map[string]string // directory node path -> fingerprint of its own type/target
	untracked map[string]string // non-ignored untracked path -> fingerprint of its type/content
}

// assertCleanReviewTree refuses a Verdict from a review run that did not leave
// the tracked worktree unchanged. A Verifier may write review.json — the one
// file it is expected to produce — but any tracked-file change between the
// before-snapshot and the run's end, any move of HEAD, any new non-review
// untracked path, or any directory that became a symlink means the agent edited
// the tree the run was meant to judge: the checks that back a Verdict would then
// describe an uncommitted experiment, never the committed Review revision. before
// must be the snapshot taken immediately before the review runs. The comparison
// is complete and symmetric — it records the working tree's type, mode and
// content, the full index entry, the parent-directory topology, and the untracked
// set per path — so editing a file that was already dirty, staging it, chmodding
// it, swapping it for an equal-content symlink, replacing a tracked directory
// with a symlink, or restoring it to clean is refused exactly as a fresh edit is.
// It returns an error naming every offending path when the tree drifted, and nil
// when only the expected review record remains. review.json is the one untracked
// file a Verifier is expected to write, so the only untracked mutation the
// comparison admits is the fresh creation of review.json; creating any other
// untracked path (such as report.json) or editing an existing untracked fixture
// in place is refused too.
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
	dirty := trackedDrift(before, after)
	keys := make(map[string]bool, len(before.untracked)+len(after.untracked))
	for path := range before.untracked {
		keys[path] = true
	}
	for path := range after.untracked {
		keys[path] = true
	}
	for _, path := range sortedPaths(keys) {
		beforeFp, hadBefore := before.untracked[path]
		afterFp, hadAfter := after.untracked[path]
		if hadBefore == hadAfter && beforeFp == afterFp {
			continue
		}
		// The one untracked mutation a review may make is to freshly create
		// review.json — the single file the Verifier is expected to write. Any
		// other untracked change — creating report.json or another artifact, a
		// fresh fixture, a renamed file, editing an existing untracked fixture in
		// place, deleting a check output — names the agent's hand in the tree, so
		// it refuses the Verdict too.
		if path == "review.json" && !hadBefore && hadAfter {
			continue
		}
		dirty = append(dirty, path)
	}
	if len(dirty) > 0 {
		sort.Strings(dirty)
		return fmt.Errorf("review left the worktree dirty: %s", strings.Join(dirty, ", "))
	}
	return nil
}

// trackedDrift returns the paths whose tracked leaf, index entry or parent
// directory node differs between two snapshots, or nil when the tracked tree is
// identical. Untracked paths are deliberately not compared here: the callers
// decide separately whether scratch output is admissible.
func trackedDrift(before, after reviewTreeSnapshot) []string {
	var dirty []string
	keys := make(map[string]bool, len(before.work)+len(after.work))
	for path := range before.work {
		keys[path] = true
	}
	for path := range after.work {
		keys[path] = true
	}
	for _, path := range sortedPaths(keys) {
		if before.work[path] != after.work[path] || before.idx[path] != after.idx[path] {
			dirty = append(dirty, path)
		}
	}
	keys = make(map[string]bool, len(before.dirs)+len(after.dirs))
	for path := range before.dirs {
		keys[path] = true
	}
	for path := range after.dirs {
		keys[path] = true
	}
	for _, dir := range sortedPaths(keys) {
		if before.dirs[dir] != after.dirs[dir] {
			dirty = append(dirty, dir)
		}
	}
	return dirty
}

// assertChecksClean refuses a pass note from a check run that did not leave the
// tracked tree unchanged. A check may create untracked scratch output, but
// rewriting a tracked file, staging an edit, swapping a tracked directory for a
// symlink, or moving HEAD means the green it reports describes an uncommitted
// experiment rather than the Review revision it was declared to test. before
// must be the snapshot taken immediately before the checks ran. It compares the
// tracked leaves, their index entries, the parent-directory topology, and HEAD,
// and ignores untracked paths, which real checks legitimately create and which
// never change the Revision. It returns an error naming every offending tracked
// path when the tree drifted, and nil when only untracked scratch output
// remains.
func assertChecksClean(wtDir, head string, before reviewTreeSnapshot) error {
	cur, err := gitOut(wtDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("checks: cannot read worktree HEAD: %w", err)
	}
	if cur != head {
		return fmt.Errorf("checks moved HEAD: reviewed %s -> %s", short(head), short(cur))
	}
	after, err := snapshotReviewTree(wtDir)
	if err != nil {
		return fmt.Errorf("checks: %w", err)
	}
	if dirty := trackedDrift(before, after); len(dirty) > 0 {
		sort.Strings(dirty)
		return fmt.Errorf("checks left the tracked tree dirty: %s", strings.Join(dirty, ", "))
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

// snapshotReviewTree captures the complete state of the worktree, not just its
// tracked leaves: the whole index entry (mode, blob, stage) and a working-tree
// fingerprint of every tracked leaf, a fingerprint of every parent directory
// node, and the fingerprint of every non-ignored untracked path. The leaf
// fingerprint records the object's type, mode and, for a regular file, its bytes
// or, for a symlink, its target, and never reads the content of a non-regular
// object, so a path a review replaced with a FIFO, device or stream cannot make
// the snapshot block. The directory fingerprints run through the same logic and
// so never follow a symlinked parent; without them the leaf bytes of a tracked
// directory swapped for a symlink would still resolve to identical content and
// hide the change. Untracked paths are read path-safely through git's NUL-based
// output so a fixture with spaces or escapes is captured whole, and each is
// fingerprinted the same way as a tracked leaf so an in-place edit of an
// existing fixture changes the comparison even though the path set does not.
func snapshotReviewTree(wtDir string) (reviewTreeSnapshot, error) {
	out, err := gitOutRaw(wtDir, "ls-files", "-s", "-z")
	if err != nil {
		return reviewTreeSnapshot{}, fmt.Errorf("review: read index: %w", err)
	}
	snap := reviewTreeSnapshot{
		work:      make(map[string]string),
		idx:       make(map[string]string),
		dirs:      make(map[string]string),
		untracked: make(map[string]string),
	}
	// "-z" emits each entry NUL-terminated and never quotes a path, so a tracked
	// file whose name holds a newline, tab or backslash is keyed exactly as git
	// tracks it. Newline-delimited "-s" output quotes such paths ("a\nb.txt"),
	// which would record the edit under a key that never matches.
	for _, entry := range strings.Split(out, "\x00") {
		if entry == "" {
			continue
		}
		// "ls-files -s" emits "<mode> <blob> <stage>\t<path>".
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			continue
		}
		// Record the whole index entry, not just the blob: git tracks the mode
		// separately, so a staged chmod (same blob, new mode) would otherwise
		// sail through the clean-tree comparison. The entry also carries a
		// gitlink's blob, so a moved submodule pointer is caught here.
		snap.idx[path] = meta
		// A gitlink's working-tree object is the checked-out submodule directory,
		// which workState would reduce to bare "type:dir" and so ignore. Its real
		// state — the submodule commit and the tracked files dirtied inside it —
		// must be fingerprinted instead, or a verifier edits a tracked file within
		// the submodule, leaves the gitlink entry and every other fingerprint
		// unchanged, and still obtains a Verdict for a merged tree that never held
		// that edit.
		if isGitlink(meta) {
			snap.work[path] = submoduleState(wtDir, path)
		} else {
			fp, err := workState(wtDir, path)
			if err != nil {
				return reviewTreeSnapshot{}, err
			}
			snap.work[path] = fp
		}
		for _, dir := range parentDirs(path) {
			if _, seen := snap.dirs[dir]; !seen {
				fp, err := workState(wtDir, dir)
				if err != nil {
					return reviewTreeSnapshot{}, err
				}
				snap.dirs[dir] = fp
			}
		}
	}
	unt, err := gitOutRaw(wtDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return reviewTreeSnapshot{}, fmt.Errorf("review: read untracked: %w", err)
	}
	for _, path := range strings.Split(unt, "\x00") {
		if path != "" {
			// Fingerprint each untracked path, not just record its name, so an
			// existing non-ignored fixture edited in place still drifts even
			// though the path set is unchanged.
			fp, err := workState(wtDir, path)
			if err != nil {
				return reviewTreeSnapshot{}, err
			}
			snap.untracked[path] = fp
		}
	}
	return snap, nil
}

// parentDirs returns the parent-directory paths of path, from the nearest
// directory up to the worktree root. It lets the snapshot fingerprint each
// directory node a tracked leaf sits under, so swapping such a directory for a
// symlink is visible even though the leaf bytes resolve to identical content.
func parentDirs(path string) []string {
	var dirs []string
	dir := filepath.Dir(path)
	for dir != "." && dir != "" && dir != string(filepath.Separator) {
		dirs = append(dirs, dir)
		dir = filepath.Dir(dir)
	}
	return dirs
}

// snapshotFileHashLimit bounds the number of content bytes workState reads from
// any one regular file, so a verifier cannot give a file a huge (possibly sparse)
// apparent length and make the snapshot stream all of it. Reading stops after
// snapshotFileHashLimit bytes, but a file whose content runs past that bound can
// no longer be fully verified, so workState fails closed rather than emit a
// fingerprint that hides a same-size mutation in the unread tail.
const snapshotFileHashLimit = 1 << 20 // 1 MiB of content per path

// workState fingerprints a tracked path's working-tree object. It captures
// everything git tracks — type, mode and, for a regular file its bytes or, for a
// symlink its target — so a chmod, a regular-file-for-symlink swap, or a symlink
// retarget all change the fingerprint even when the read bytes would not. It
// stats with Lstat, never the following stat, and reads content only from a real
// regular file, streamed through a buffer capped at snapshotFileHashLimit bytes,
// or from a symlink's target, so a path replaced by a FIFO, socket, device, an
// endless stream, or a huge sparse file is fingerprinted without blocking or
// exhausting the factory. A regular leaf under a symlinked parent is fingerprinted
// by its own type and mode without following the link, so a verifier cannot point
// a tracked directory at a huge external file and stall the refusal. It never
// emits a fingerprint for content it could not read in full: a regular file larger
// than snapshotFileHashLimit returns an error, because no bounded fingerprint can
// prove such a file unchanged (a same-size mutation beyond the read prefix would
// share every recorded field). "" means the tracked path is absent, i.e. it was
// deleted, which is as much a change as a rewrite is.
func workState(wtDir, path string) (string, error) {
	full := filepath.Join(wtDir, path)
	fi, err := os.Lstat(full)
	if err != nil {
		return "", nil
	}
	mode := fi.Mode()
	h := sha1.New()
	fmt.Fprintf(h, "%04o", mode.Perm()) // the mode bits, so chmod changes the hash
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			target = "\x00unreadable"
		}
		fmt.Fprintf(h, "link:%s", target)
	case mode&os.ModeNamedPipe != 0:
		h.Write([]byte("type:fifo"))
	case mode&os.ModeSocket != 0:
		h.Write([]byte("type:socket"))
	case mode&os.ModeDevice != 0:
		h.Write([]byte("type:device"))
	case mode.IsDir():
		h.Write([]byte("type:dir"))
	case mode.IsRegular():
		if symlinkedAncestor(wtDir, path) {
			// Reading the leaf would resolve through a symlinked parent into its
			// target, a huge external file a verifier could plant to stall or
			// exhaust the factory before the refusal is emitted. Refuse to follow
			// it: fingerprint the leaf's own type and mode, which still changes
			// when the topology above it does.
			fmt.Fprintf(h, "type:file;via:symlinked-parent;perm:%04o", mode.Perm())
			break
		}
		f, err := os.Open(full)
		if err != nil {
			fmt.Fprintf(h, "type:file;unreadable")
			break
		}
		fmt.Fprintf(h, "type:file;perm:%04o;data:", mode.Perm())
		// Read one byte past the limit so a file whose content runs on beyond the
		// bounded prefix is detected without ever streaming the whole tail. For a
		// file up to the limit this hashes its complete content, so an edit
		// anywhere in an ordinary source file changes the digest.
		n, copyErr := io.Copy(h, io.LimitReader(f, snapshotFileHashLimit+1))
		f.Close()
		if copyErr != nil {
			fmt.Fprintf(h, ";read-error")
			break
		}
		if n > snapshotFileHashLimit {
			// Content extends beyond the bytes we hashed; a tracked edit confined
			// to that tail would share this identical prefix, so the fingerprint
			// cannot prove the file unchanged. Fail closed instead of silently
			// accepting the escape: refuse the Verdict because this path cannot be
			// fully verified. The read stopped at the limit, so a hostile huge
			// sparse file cannot stall the snapshot.
			return "", fmt.Errorf("path %q cannot be fully fingerprinted: %d bytes exceed the %d-byte content limit", path, fi.Size(), snapshotFileHashLimit)
		}
		fmt.Fprintf(h, ";size:%d", fi.Size())
	default:
		fmt.Fprintf(h, "type:%d", mode)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// isGitlink reports whether an `ls-files -s` index entry is a submodule (gitlink)
// reference. Git records a gitlink with index mode 160000, whose working-tree
// object is the checked-out submodule directory rather than a normal file.
func isGitlink(meta string) bool {
	return strings.HasPrefix(meta, "160000")
}

// submoduleState fingerprints a checked-out submodule's real state, the part of
// the worktree a gitlink index entry alone cannot see. The parent repository
// records only the submodule commit; the tracked files dirtied inside the
// checked-out submodule — the ones a check or review actually reads and runs —
// are invisible to the parent's index and to workState, which would flatten the
// submodule path to bare "type:dir". Its own checked-out commit is captured so a
// commit inside the submodule drifts even when the parent gitlink entry is
// unchanged, and its `status --porcelain` is captured so editing, staging, or
// deleting a tracked file inside the submodule changes the fingerprint and the
// clean-tree assertion refuses the Verdict. Untracked scratch inside the
// submodule is ignored because real checks may create it, mirroring how
// assertChecksClean treats untracked paths in the parent. "" means the submodule
// directory is absent, i.e. it was deleted, which is as much a change as any
// edit in it is.
func submoduleState(wtDir, path string) string {
	sub := filepath.Join(wtDir, path)
	h := sha1.New()
	head, err := gitOut(sub, "rev-parse", "HEAD")
	if err != nil {
		// The submodule is not a working git repo (missing or uninitialised); its
		// absence from git's view is itself a state that must compare stably.
		fmt.Fprintf(h, "no-git-repo")
		return hex.EncodeToString(h.Sum(nil))
	}
	fmt.Fprintf(h, "head:%s;", head)
	status, err := gitOutRaw(sub, "status", "--porcelain")
	if err != nil {
		fmt.Fprintf(h, "status-error:%v", err)
		return hex.EncodeToString(h.Sum(nil))
	}
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimRight(line, "\r")
		if len(line) < 2 {
			continue
		}
		// The first two columns are the index/working-tree status (" M " for a
		// modified tracked file); "??" marks untracked scratch, which real checks
		// may legitimately leave behind and which never changes the merged tree.
		if strings.HasPrefix(line[:2], "??") {
			continue
		}
		fmt.Fprintf(h, "%s;", line)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// symlinkedAncestor reports whether any parent directory of rel, walking from
// the worktree root down, is a symlink. os.Lstat only avoids following the final
// component of a path, so a leaf beneath a symlinked parent would otherwise be
// resolved through it into the link target. Refusing to follow keeps a verifier
// from pointing a tracked directory at a huge external file and stalling or
// exhausting the factory before the clean-tree refusal is emitted. It returns
// false when a parent no longer exists, because the leaf that names it then
// fingerprints as absent on its own.
func symlinkedAncestor(wtDir, rel string) bool {
	dir := filepath.Dir(rel)
	for dir != "." && dir != "" && dir != string(filepath.Separator) {
		fi, err := os.Lstat(filepath.Join(wtDir, dir))
		if err != nil {
			return false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return true
		}
		dir = filepath.Dir(dir)
	}
	return false
}

// checkSchema validates a JSON run artifact against its declared JSON Schema:
// the file parses, and every required property is present and non-empty.
func checkSchema(file, schemaPath string) error {
	sb, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("read schema %s: %w", schemaPath, err)
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
