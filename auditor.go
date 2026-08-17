package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type AuditState struct {
	Baseline      string   `json:"baseline"`
	LastMaster    string   `json:"last_master"`
	AuditedMaster string   `json:"audited_master"`
	LastAt        string   `json:"last_at"`
	LastResult    string   `json:"last_result"`
	Violations    []string `json:"violations"`
}

type auditSnapshot struct {
	Master   string
	Notes    map[string]string
	Evidence map[string]string
	id       string
}

const (
	auditorNotesNamespace  = "refs/forest/private/audit"
	auditorMasterNamespace = "refs/heads/forest-audit"
	auditSnapshotAttempts  = 3
	auditHistoryEntries    = 1000
	auditHistoryEntryBytes = 64 * 1024
	auditLogTempPrefix     = ".audit.log-"

	auditorCapacityEntries     = 500
	auditorConcreteViolations  = 999
	auditorViolationEntryBytes = 1 << 10
)

type AuditResult struct {
	Master     string
	Advanced   bool
	Violations []string
}

type auditDependencies struct {
	runGit   func(context.Context, string, ...string) ([]byte, error)
	syncFile func(*os.File) error
}

func defaultAuditDependencies() auditDependencies {
	return auditDependencies{
		runGit:   runAuditGit,
		syncFile: (*os.File).Sync,
	}
}

func auditStatePath(root string) string { return forestPath(root, "audit.json") }
func auditLogPath(root string) string   { return forestPath(root, "audit.log") }

func audit(ctx context.Context, root string) (AuditResult, error) {
	return auditWithDependencies(ctx, root, defaultAuditDependencies())
}

func auditWithDependencies(ctx context.Context, root string, deps auditDependencies) (result AuditResult, err error) {
	snapshot, err := newAuditSnapshot()
	if err != nil {
		return AuditResult{}, fmt.Errorf("create audit snapshot: %w", err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			err = errors.Join(err, clearEvidenceSnapshot(root, snapshot, deps))
		}
	}()
	snapshot, err = fetchEvidenceAuditSnapshot(ctx, root, snapshot, deps)
	if err != nil {
		return AuditResult{}, fmt.Errorf("fetch forest evidence: %w", err)
	}
	master := snapshot.Master
	state, err := readAuditState(root)
	if err != nil {
		return AuditResult{}, err
	}
	if state.Baseline == "" {
		state.Baseline = master
		if err := ensureAuditWorkspace(root, deps); err != nil {
			return AuditResult{}, err
		}
		if err := writeAuditState(root, state, deps); err != nil {
			return AuditResult{}, err
		}
	}
	configData, err := deps.runGit(ctx, root, "show", master+":forest.yaml")
	if err != nil {
		return AuditResult{}, fmt.Errorf("read forest.yaml at %s: %w", master, err)
	}
	cfg, err := decodeConfig(configData, master+":forest.yaml")
	if err != nil {
		return AuditResult{}, err
	}
	entries, enumerationViolations, err := readEvidence(ctx, root, snapshot, deps)
	if err != nil {
		return AuditResult{}, err
	}
	var violations violationCollector
	for _, violation := range enumerationViolations {
		violations.add(violation)
	}
	if cleanupErr := clearEvidenceSnapshot(root, snapshot, deps); cleanupErr != nil {
		return AuditResult{}, cleanupErr
	}
	cleaned = true

	anchor := state.LastMaster
	if anchor == "" {
		anchor = state.Baseline
	}
	advanced := anchor != "" && anchor != master
	result = AuditResult{Master: master, Advanced: advanced, Violations: nil}
	if advanced {
		if _, ancestryErr := deps.runGit(ctx, root, "merge-base", "--is-ancestor", anchor, master); ancestryErr != nil {
			if !soleExitCode(ancestryErr, 1) {
				return AuditResult{}, fmt.Errorf("check master ancestry: %w", ancestryErr)
			}
			violations.add("master advanced non-fast-forward from " + anchor + " to " + master)
		}
	}
	if master != state.Baseline {
		if err := verifyEvidenceGate(entries, master, cfg); err != nil {
			violations.add(err.Error())
		}
	}
	result.Violations = violations.finalize()
	if err := ensureAuditWorkspace(root, deps); err != nil {
		return result, err
	}
	state.LastAt = time.Now().UTC().Format(time.RFC3339Nano)
	state.AuditedMaster = master
	if len(result.Violations) == 0 {
		state.LastMaster = master
		state.LastResult = "pass"
		state.Violations = nil
	} else {
		changed := !sameViolationSet(state.Violations, result.Violations)
		state.LastResult = "violations"
		state.Violations = result.Violations
		if changed {
			if err := appendAuditLog(ctx, root, result.Violations, deps); err != nil {
				return result, err
			}
		}
	}
	if err := writeAuditState(root, state, deps); err != nil {
		return result, err
	}
	return result, nil
}

func ensureAuditWorkspace(root string, deps auditDependencies) error {
	path := filepath.Join(root, workspaceName)
	if err := os.Mkdir(path, 0o755); err != nil {
		if !os.IsExist(err) {
			return err
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	}
	directory, err := os.Open(root)
	if err != nil {
		return fmt.Errorf("open repository root: %w", err)
	}
	return syncAndCloseAuditFile(directory, "repository root", deps)
}

// readAuditState reads persisted audit state. Violations is always a slice so
// the published payload never carries null in place of an empty list.
func readAuditState(root string) (AuditState, error) {
	empty := AuditState{Violations: []string{}}
	data, err := os.ReadFile(auditStatePath(root))
	if os.IsNotExist(err) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	var state AuditState
	if err := json.Unmarshal(data, &state); err != nil {
		return empty, fmt.Errorf("parse audit state: %w", err)
	}
	if state.Baseline == "" && state.LastMaster != "" {
		state.Baseline = state.LastMaster
	}
	if state.Violations == nil {
		state.Violations = []string{}
	}
	return state, nil
}

func writeAuditState(root string, state AuditState, deps auditDependencies) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	path := auditStatePath(root)
	file, err := os.CreateTemp(filepath.Dir(path), ".audit.json-*")
	if err != nil {
		return fmt.Errorf("create audit state temp: %w", err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return fmt.Errorf("set audit state permissions: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("write audit state: %w", err)
	}
	if err := syncAndCloseAuditFile(file, "audit state", deps); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace audit state: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open audit directory: %w", err)
	}
	return syncAndCloseAuditFile(directory, "audit directory", deps)
}

func syncAndCloseAuditFile(file *os.File, name string, deps auditDependencies) error {
	if err := deps.syncFile(file); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return nil
}

func sameViolationSet(left, right []string) bool {
	leftSet := make(map[string]struct{}, len(left))
	rightSet := make(map[string]struct{}, len(right))
	for _, violation := range left {
		leftSet[violation] = struct{}{}
	}
	for _, violation := range right {
		rightSet[violation] = struct{}{}
	}
	return maps.Equal(leftSet, rightSet)
}

// violationCollector bounds the concrete violations an Audit retains. It keeps
// at most auditorConcreteViolations concrete entries and counts any excess so
// finalize can report one exact omission summary.
type violationCollector struct {
	entries []string
	omitted int
}

func (c *violationCollector) add(violation string) {
	if violation == "" {
		return
	}
	if len(violation) > auditorViolationEntryBytes {
		violation = truncateViolation(violation)
	}
	if len(c.entries) < auditorConcreteViolations {
		c.entries = append(c.entries, violation)
		return
	}
	c.omitted++
}

func (c *violationCollector) finalize() []string {
	if c.omitted == 0 {
		return c.entries
	}
	result := make([]string, 0, len(c.entries)+1)
	result = append(result, c.entries...)
	result = append(result, fmt.Sprintf("%d additional violations omitted", c.omitted))
	return result
}

func truncateViolation(violation string) string {
	if len(violation) <= auditorViolationEntryBytes {
		return violation
	}
	truncated := violation[:auditorViolationEntryBytes]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}
	return truncated
}

func auditCapacityViolation(ref string) string {
	return fmt.Sprintf("note ref %s exceeds %d-entry audit enumeration capacity", ref, auditorCapacityEntries)
}

func auditPayloadViolation(ref, revision string) string {
	return fmt.Sprintf("note payload on %s for %s exceeds %d-byte limit", ref, revision, auditHistoryEntryBytes)
}

func auditShowOverflowViolation(ref, revision string) string {
	return fmt.Sprintf("note transport output overflow on %s for %s", ref, revision)
}

func auditMissingNoteViolation(ref, revision string) string {
	return fmt.Sprintf("missing note on %s for %s", ref, revision)
}

func auditMissingNoteObjectViolation(ref, revision string) string {
	return fmt.Sprintf("missing note object on %s for %s", ref, revision)
}

func appendAuditLog(ctx context.Context, root string, violations []string, deps auditDependencies) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := auditLogPath(root)
	if err := cleanupAuditLogTemps(path, deps); err != nil {
		return err
	}

	history := make([]string, 0, auditHistoryEntries)
	next := 0
	add := func(entry string) {
		if len(history) < auditHistoryEntries {
			history = append(history, entry)
			return
		}
		history[next] = entry
		next = (next + 1) % auditHistoryEntries
	}
	if err := scanAuditLog(ctx, path, add); err != nil {
		return err
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, violation := range violations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.ContainsAny(violation, "\r\n") {
			return errors.New("audit violation contains a line break")
		}
		if len(violation) > auditHistoryEntryBytes-len(timestamp)-1 {
			return fmt.Errorf("audit violation exceeds %d-byte history entry limit", auditHistoryEntryBytes)
		}
		add(timestamp + " " + violation)
	}

	file, err := os.CreateTemp(filepath.Dir(path), auditLogTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create audit log temp: %w", err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err := file.Chmod(0o644); err != nil {
		_ = file.Close()
		return fmt.Errorf("set audit log permissions: %w", err)
	}
	for index := range history {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return err
		}
		entry := history[(next+index)%len(history)]
		if _, err := fmt.Fprintln(file, entry); err != nil {
			_ = file.Close()
			return fmt.Errorf("write audit log: %w", err)
		}
	}
	if err := syncAndCloseAuditFile(file, "audit log", deps); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace audit log: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open audit directory: %w", err)
	}
	return syncAndCloseAuditFile(directory, "audit directory", deps)
}

func scanAuditLog(ctx context.Context, path string, visit func(string)) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open audit log: %w", err)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), auditHistoryEntryBytes+2)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			_ = file.Close()
			return err
		}
		entry := scanner.Text()
		if len(entry) > auditHistoryEntryBytes {
			_ = file.Close()
			return fmt.Errorf("audit history entry exceeds %d-byte limit", auditHistoryEntryBytes)
		}
		visit(entry)
	}
	return errors.Join(scanner.Err(), file.Close())
}

// ReadAuditLog returns the newest audit history entries, oldest first, bounded
// by limit. A limit of zero or less means the whole retained history.
func ReadAuditLog(ctx context.Context, root string, limit int) ([]string, error) {
	if limit <= 0 || limit > auditHistoryEntries {
		limit = auditHistoryEntries
	}
	entries := make([]string, 0, limit)
	next := 0
	if err := scanAuditLog(ctx, auditLogPath(root), func(entry string) {
		if len(entries) < limit {
			entries = append(entries, entry)
			return
		}
		entries[next] = entry
		next = (next + 1) % limit
	}); err != nil {
		return nil, err
	}
	if next != 0 {
		slices.Reverse(entries[:next])
		slices.Reverse(entries[next:])
		slices.Reverse(entries)
	}
	return entries, nil
}

func cleanupAuditLogTemps(path string, deps auditDependencies) error {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read audit directory: %w", err)
	}
	var cleanupErr error
	removed := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), auditLogTempPrefix) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
			continue
		}
		removed = true
	}
	if !removed {
		return cleanupErr
	}
	directory, err := os.Open(dir)
	if err != nil {
		return errors.Join(cleanupErr, fmt.Errorf("open audit directory: %w", err))
	}
	return errors.Join(cleanupErr, syncAndCloseAuditFile(directory, "audit directory", deps))
}

func newAuditSnapshot() (auditSnapshot, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return auditSnapshot{}, err
	}
	return auditSnapshot{
		Notes:    map[string]string{},
		Evidence: map[string]string{},
		id:       fmt.Sprintf("%x", id[:]),
	}, nil
}

func auditorNoteRef(snapshot auditSnapshot, ref string) string {
	return auditorNotesNamespace + "/" + snapshot.id + "/" + strings.TrimPrefix(ref, "refs/notes/forest/")
}

func auditorMasterRef(snapshot auditSnapshot) string {
	return auditorMasterNamespace + "/" + snapshot.id + "/master"
}

func readNotes(ctx context.Context, root string, snapshot auditSnapshot, deps auditDependencies) ([]noteEntry, []string, error) {
	var entries []noteEntry
	var violations []string
	for _, ref := range coordinationNoteRefs() {
		if snapshot.Notes[ref] == "" {
			continue
		}
		snapshotRef := auditorNoteRef(snapshot, ref)
		list, err := deps.runGit(ctx, root, "notes", "--ref="+snapshotRef, "list")
		if err != nil {
			if errors.Is(err, errTrustedTransportOutputOverflow) {
				violations = append(violations, auditCapacityViolation(ref))
				continue
			}
			return nil, nil, err
		}
		pathsByRevision, treeViolations, err := notePaths(ctx, root, snapshotRef, ref, deps)
		if err != nil {
			if errors.Is(err, errTrustedTransportOutputOverflow) {
				violations = append(violations, auditCapacityViolation(ref))
				continue
			}
			return nil, nil, err
		}
		violations = append(violations, treeViolations...)
		text := strings.TrimSpace(string(list))
		if text == "" {
			if len(pathsByRevision) != 0 {
				violations = append(violations, fmt.Sprintf("unexpected note tree entry on %s", ref))
			}
			continue
		}
		lines := strings.Split(text, "\n")
		if len(lines) > auditorCapacityEntries || len(pathsByRevision) > auditorCapacityEntries {
			violations = append(violations, auditCapacityViolation(ref))
			continue
		}
		for _, line := range lines {
			fields := strings.Fields(line)
			if len(fields) != 2 || !isSHA(fields[0]) || !isSHA(fields[1]) {
				violations = append(violations, fmt.Sprintf("malformed note list on %s", ref))
				continue
			}
			treeEntry, ok := pathsByRevision[fields[1]]
			if !ok {
				violations = append(violations, fmt.Sprintf("missing note tree entry for %s on %s", fields[1], ref))
				continue
			}
			if treeEntry.blob != fields[0] {
				violations = append(violations, fmt.Sprintf("note blob mismatch for %s on %s", fields[1], ref))
				delete(pathsByRevision, fields[1])
				continue
			}
			delete(pathsByRevision, fields[1])
			payload, err := deps.runGit(ctx, root, "notes", "--ref="+snapshotRef, "show", fields[1])
			if err != nil {
				if errors.Is(err, errTrustedTransportOutputOverflow) {
					violations = append(violations, auditShowOverflowViolation(ref, fields[1]))
					continue
				}
				if soleExitCode(err, 1) {
					violations = append(violations, auditMissingNoteViolation(ref, fields[1]))
					continue
				}
				if soleExitCode(err, 128) {
					violations = append(violations, auditMissingNoteObjectViolation(ref, fields[1]))
					continue
				}
				return nil, nil, err
			}
			if len(payload) > auditHistoryEntryBytes {
				violations = append(violations, auditPayloadViolation(ref, fields[1]))
				continue
			}
			author, email, err := noteAuthor(ctx, root, snapshotRef, treeEntry.path, deps)
			if err != nil {
				return nil, nil, fmt.Errorf("read note author on %s for %s: %w", ref, fields[1], err)
			}
			entries = append(entries, noteEntry{Ref: ref, Revision: fields[1], Payload: payload, Author: author, Email: email})
		}
		if len(pathsByRevision) != 0 {
			violations = append(violations, fmt.Sprintf("unexpected note tree entry on %s", ref))
		}
	}
	return entries, violations, nil
}

type noteTreeEntry struct {
	blob string
	path string
}

// notePaths reads the note tree at gitRef and returns tree entries keyed by
// revision. Corrupt rows are reported as note-specific bounded violations
// named for messageRef and skipped so valid rows still enumerate. Transport,
// process, pipe, and deadline failures from runGit stay operational errors.
func notePaths(ctx context.Context, root, gitRef, messageRef string, deps auditDependencies) (map[string]noteTreeEntry, []string, error) {
	output, err := deps.runGit(ctx, root, "ls-tree", "-r", "--full-tree", gitRef)
	if err != nil {
		return nil, nil, err
	}
	paths := make(map[string]noteTreeEntry)
	var violations []string
	text := string(output)
	if text == "" {
		return paths, violations, nil
	}
	text = strings.TrimSuffix(text, "\n")
	for _, line := range strings.Split(text, "\n") {
		left, path, ok := strings.Cut(line, "\t")
		if !ok || path == "" || strings.Contains(path, "\t") {
			violations = append(violations, fmt.Sprintf("malformed note tree row on %s", messageRef))
			continue
		}
		fields := strings.Split(left, " ")
		if len(fields) != 3 {
			violations = append(violations, fmt.Sprintf("malformed note tree row on %s", messageRef))
			continue
		}
		if fields[0] != "100644" || fields[1] != "blob" {
			violations = append(violations, fmt.Sprintf("non-blob note tree entry on %s: %s", messageRef, path))
			continue
		}
		if !isSHA(fields[2]) {
			violations = append(violations, fmt.Sprintf("malformed note tree row on %s", messageRef))
			continue
		}
		revision := strings.ReplaceAll(path, "/", "")
		if !isSHA(revision) {
			violations = append(violations, fmt.Sprintf("non-SHA note tree path on %s: %s", messageRef, path))
			continue
		}
		if previous, ok := paths[revision]; ok {
			violations = append(violations, fmt.Sprintf("duplicate note paths for %s on %s: %s and %s", revision, messageRef, previous.path, path))
			continue
		}
		paths[revision] = noteTreeEntry{blob: fields[2], path: path}
	}
	return paths, violations, nil
}

func noteAuthor(ctx context.Context, root, ref, path string, deps auditDependencies) (string, string, error) {
	output, err := deps.runGit(ctx, root, "log", "-1", "--format=%an%x00%ae", ref, "--", path)
	if err != nil {
		return "", "", err
	}
	record, err := exactGitLine(output)
	if err != nil {
		return "", "", fmt.Errorf("invalid note author for %s:%s: %w", ref, path, err)
	}
	parts := strings.SplitN(record, "\x00", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("missing note author for %s:%s", ref, path)
	}
	return parts[0], parts[1], nil
}

func verifyGate(entries []noteEntry, master string, cfg Config) error {
	var reviewRequest noteEntry
	var reviewRequestCount int
	var approved bool
	var checksNotes []checksNote
	for _, entry := range entries {
		if entry.Revision != master {
			continue
		}
		switch entry.Ref {
		case reviewRequestNoteRef:
			reviewRequest = entry
			reviewRequestCount++
		case verdictNoteRef:
			if note, err := decodeVerdict(entry.Payload, master); err == nil && note.Verdict == "approve" {
				approved = true
			}
		case checksNoteRef:
			if note, err := decodeChecks(entry.Payload, master); err == nil {
				checksNotes = append(checksNotes, note)
			}
		}
	}
	if reviewRequestCount != 1 {
		return fmt.Errorf("master %s does not have exactly one valid review-request note", master)
	}
	if _, err := decodeReview(reviewRequest.Payload, master); err != nil || !validIdentity(reviewRequest, "builder", "fixer") {
		return fmt.Errorf("master %s does not have exactly one valid review-request note", master)
	}
	if !approved {
		return fmt.Errorf("master %s has no approve verdict note", master)
	}
	if len(cfg.Checks) == 0 {
		return fmt.Errorf("master %s has no configured checks", master)
	}
	if len(checksNotes) != 1 {
		return fmt.Errorf("master %s has no passing checks note", master)
	}
	reported := checksNotes[0].Results
	if len(reported) == 0 {
		return fmt.Errorf("master %s has an empty checks note", master)
	}
	if len(reported) != len(cfg.Checks) {
		return fmt.Errorf("master %s checks note does not match configured checks", master)
	}
	for i, check := range cfg.Checks {
		result := reported[i]
		if result.Name != check.Name {
			return fmt.Errorf("master %s checks note does not match configured checks", master)
		}
		if !result.OK || result.Exit != 0 {
			return fmt.Errorf("master %s has no passing checks note", master)
		}
	}
	return nil
}

func fetchAuditSnapshot(ctx context.Context, root string, acquisition auditSnapshot, deps auditDependencies) (auditSnapshot, error) {
	for attempt := range auditSnapshotAttempts {
		advertised, err := advertiseAuditSnapshot(ctx, root, deps)
		if err != nil {
			return acquisition, err
		}
		advertised.id = acquisition.id
		if err := fetchSnapshotRefs(ctx, root, advertised, deps); err != nil {
			if cleanupErr := clearAuditSnapshot(root, acquisition, deps); cleanupErr != nil {
				return acquisition, errors.Join(err, cleanupErr)
			}
			if attempt+1 < auditSnapshotAttempts {
				continue
			}
			return acquisition, err
		}
		observed, err := advertiseAuditSnapshot(ctx, root, deps)
		if err != nil {
			return acquisition, err
		}
		observed.id = acquisition.id
		if sameAuditSnapshot(advertised, observed) {
			return observed, nil
		}
		if err := clearAuditSnapshot(root, acquisition, deps); err != nil {
			return acquisition, err
		}
	}
	return acquisition, fmt.Errorf("remote snapshot changed during audit")
}

func advertiseAuditSnapshot(ctx context.Context, root string, deps auditDependencies) (auditSnapshot, error) {
	refs := coordinationNoteRefs()
	args := append([]string{"ls-remote", "origin", "refs/heads/master"}, refs...)
	output, err := deps.runGit(ctx, root, args...)
	if err != nil {
		return auditSnapshot{}, fmt.Errorf("read remote snapshot: %w", err)
	}
	snapshot := auditSnapshot{Notes: make(map[string]string, len(refs))}
	for _, ref := range refs {
		snapshot.Notes[ref] = ""
	}
	seen := make(map[string]bool, len(refs)+1)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !isSHA(fields[0]) || seen[fields[1]] {
			return auditSnapshot{}, fmt.Errorf("malformed remote snapshot")
		}
		ref := fields[1]
		if ref == "refs/heads/master" {
			snapshot.Master = fields[0]
		} else {
			if _, ok := snapshot.Notes[ref]; !ok {
				return auditSnapshot{}, fmt.Errorf("malformed remote snapshot ref %s", ref)
			}
			snapshot.Notes[ref] = fields[0]
		}
		seen[ref] = true
	}
	if snapshot.Master == "" {
		return auditSnapshot{}, fmt.Errorf("origin/master is missing or malformed")
	}
	return snapshot, nil
}

func sameAuditSnapshot(left, right auditSnapshot) bool {
	if left.Master != right.Master {
		return false
	}
	for _, ref := range coordinationNoteRefs() {
		if left.Notes[ref] != right.Notes[ref] {
			return false
		}
	}
	return true
}

func fetchSnapshotRefs(ctx context.Context, root string, snapshot auditSnapshot, deps auditDependencies) error {
	if err := fetchSnapshotRef(ctx, root, "refs/heads/master", auditorMasterRef(snapshot), snapshot.Master, deps); err != nil {
		return err
	}
	for _, ref := range coordinationNoteRefs() {
		if err := fetchSnapshotRef(ctx, root, ref, auditorNoteRef(snapshot, ref), snapshot.Notes[ref], deps); err != nil {
			return err
		}
	}
	return nil
}

func fetchSnapshotRef(ctx context.Context, root, source, destination, oid string, deps auditDependencies) error {
	if oid == "" {
		if _, err := deps.runGit(ctx, root, "update-ref", "-d", destination); err != nil {
			return fmt.Errorf("clear absent %s: %w", source, err)
		}
		return nil
	}
	if _, err := deps.runGit(ctx, root, "fetch", "--no-tags", "origin", source+":"+destination); err != nil {
		return fmt.Errorf("fetch %s: %w", source, err)
	}
	if err := verifyAuditRef(ctx, root, destination, oid, deps); err != nil {
		return err
	}
	return nil
}

func verifyAuditRef(ctx context.Context, root, ref, expected string, deps auditDependencies) error {
	output, err := deps.runGit(ctx, root, "for-each-ref", "--format=%(objectname)", ref)
	if err != nil {
		return fmt.Errorf("verify fetched %s: %w", ref, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 1 || fields[0] != expected {
		return fmt.Errorf("fetched %s does not match advertised object", ref)
	}
	return nil
}

func clearAuditSnapshot(root string, snapshot auditSnapshot, deps auditDependencies) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var cleanup error
	for _, ref := range coordinationNoteRefs() {
		privateRef := auditorNoteRef(snapshot, ref)
		if _, err := deps.runGit(ctx, root, "update-ref", "-d", privateRef); err != nil {
			cleanup = errors.Join(cleanup, fmt.Errorf("clear %s: %w", privateRef, err))
		}
	}
	privateMaster := auditorMasterRef(snapshot)
	if _, err := deps.runGit(ctx, root, "update-ref", "-d", privateMaster); err != nil {
		cleanup = errors.Join(cleanup, fmt.Errorf("clear %s: %w", privateMaster, err))
	}
	return cleanup
}

func runAuditGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	path, err := trustedExecutable(root, "git")
	if err != nil {
		return nil, err
	}
	return processGroupOutput(ctx, exec.Command(path, append([]string{"-C", root}, args...)...))
}
