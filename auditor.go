package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type AuditState struct {
	Baseline   string   `json:"baseline"`
	LastMaster string   `json:"last_master"`
	LastAt     string   `json:"last_at"`
	LastResult string   `json:"last_result"`
	Violations []string `json:"violations"`
}

type auditSnapshot struct {
	Master string
	Notes  map[string]string
	id     string
}

const (
	auditorNotesNamespace  = "refs/notes/forest-audit"
	auditorMasterNamespace = "refs/heads/forest-audit"
	auditSnapshotAttempts  = 3
)

type AuditResult struct {
	Master     string
	Advanced   bool
	Violations []string
}

func auditStatePath(root string) string { return forestPath(root, "audit.json") }
func auditLogPath(root string) string   { return forestPath(root, "audit.log") }

var syncAuditFile = (*os.File).Sync

func Audit(root string) (AuditResult, error) { return audit(context.Background(), root) }

func audit(ctx context.Context, root string) (result AuditResult, err error) {
	snapshot, err := newAuditSnapshot()
	if err != nil {
		return AuditResult{}, fmt.Errorf("create audit snapshot: %w", err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			err = errors.Join(err, clearAuditSnapshot(root, snapshot))
		}
	}()
	snapshot, err = fetchAuditSnapshot(ctx, root, snapshot)
	if err != nil {
		return AuditResult{}, fmt.Errorf("fetch forest notes: %w", err)
	}
	master := snapshot.Master
	configData, err := gitRun(ctx, root, "show", master+":forest.yaml")
	if err != nil {
		return AuditResult{}, fmt.Errorf("read forest.yaml at %s: %w", master, err)
	}
	cfg, err := decodeConfig(configData, master+":forest.yaml")
	if err != nil {
		return AuditResult{}, err
	}
	entries, err := readNotes(ctx, root, snapshot)
	if err != nil {
		return AuditResult{}, err
	}
	var noteViolations []string
	for _, entry := range entries {
		if err := validateNoteEntry(entry); err != nil {
			noteViolations = append(noteViolations, err.Error())
		}
	}
	if cleanupErr := clearAuditSnapshot(root, snapshot); cleanupErr != nil {
		return AuditResult{}, cleanupErr
	}
	cleaned = true

	state, err := readAuditState(root)
	if err != nil {
		return AuditResult{}, err
	}
	if state.Baseline == "" {
		state.Baseline = master
	}
	anchor := state.LastMaster
	if anchor == "" {
		anchor = state.Baseline
	}
	result = AuditResult{Master: master, Advanced: anchor != "" && anchor != master, Violations: noteViolations}
	if result.Advanced {
		if _, ancestryErr := gitRun(ctx, root, "merge-base", "--is-ancestor", anchor, master); ancestryErr != nil {
			var exitErr *exec.ExitError
			if errors.Is(ancestryErr, context.Canceled) || errors.Is(ancestryErr, context.DeadlineExceeded) || !errors.As(ancestryErr, &exitErr) || exitErr.ExitCode() != 1 {
				return result, fmt.Errorf("check master ancestry: %w", ancestryErr)
			}
			result.Violations = append(result.Violations, "master advanced non-fast-forward from "+anchor+" to "+master)
		}
	}
	if master != state.Baseline {
		if err := verifyGate(entries, master, cfg); err != nil {
			result.Violations = append(result.Violations, err.Error())
		}
	}
	if err := ensureAuditWorkspace(root); err != nil {
		return result, err
	}
	state.LastAt = time.Now().UTC().Format(time.RFC3339Nano)
	if len(result.Violations) == 0 {
		state.LastMaster = master
		state.LastResult = "pass"
		state.Violations = nil
	} else {
		state.LastResult = "violations"
		state.Violations = append([]string(nil), result.Violations...)
		if err := appendAuditLog(root, result.Violations); err != nil {
			return result, err
		}
	}
	if err := writeAuditState(root, state); err != nil {
		return result, err
	}
	return result, nil
}

func ensureAuditWorkspace(root string) error {
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
	return syncAndCloseAuditFile(directory, "repository root")
}

func readAuditState(root string) (AuditState, error) {
	data, err := os.ReadFile(auditStatePath(root))
	if os.IsNotExist(err) {
		return AuditState{}, nil
	}
	if err != nil {
		return AuditState{}, err
	}
	var state AuditState
	if err := json.Unmarshal(data, &state); err != nil {
		return AuditState{}, fmt.Errorf("parse audit state: %w", err)
	}
	if state.Baseline == "" && state.LastMaster != "" {
		state.Baseline = state.LastMaster
	}
	return state, nil
}

func writeAuditState(root string, state AuditState) error {
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
	if err := syncAndCloseAuditFile(file, "audit state"); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace audit state: %w", err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open audit directory: %w", err)
	}
	return syncAndCloseAuditFile(directory, "audit directory")
}

func syncAndCloseAuditFile(file *os.File, name string) error {
	if err := syncAuditFile(file); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s: %w", name, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", name, err)
	}
	return nil
}

func appendAuditLog(root string, violations []string) error {
	file, err := os.OpenFile(auditLogPath(root), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	for _, violation := range violations {
		if _, err := fmt.Fprintf(file, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), violation); err != nil {
			_ = file.Close()
			return err
		}
	}
	return syncAndCloseAuditFile(file, "audit log")
}

type noteEntry struct {
	Ref      string
	Revision string
	Payload  []byte
	Author   string
	Email    string
}

func newAuditSnapshot() (auditSnapshot, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return auditSnapshot{}, err
	}
	return auditSnapshot{
		Notes: make(map[string]string, len(forestNoteRefs)),
		id:    fmt.Sprintf("%x", id[:]),
	}, nil
}

func auditorNoteRef(snapshot auditSnapshot, ref string) string {
	return auditorNotesNamespace + "/" + snapshot.id + "/" + strings.TrimPrefix(ref, "refs/notes/forest/")
}

func auditorMasterRef(snapshot auditSnapshot) string {
	return auditorMasterNamespace + "/" + snapshot.id + "/master"
}

func readNotes(ctx context.Context, root string, snapshot auditSnapshot) ([]noteEntry, error) {
	var entries []noteEntry
	for _, ref := range forestNoteRefs {
		if snapshot.Notes[ref] == "" {
			continue
		}
		snapshotRef := auditorNoteRef(snapshot, ref)
		list, err := gitRun(ctx, root, "notes", "--ref="+snapshotRef, "list")
		if err != nil {
			return nil, err
		}
		text := strings.TrimSpace(string(list))
		if text == "" {
			continue
		}
		pathsByRevision, err := notePaths(ctx, root, snapshotRef)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(text, "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || !isSHA(fields[0]) || !isSHA(fields[1]) {
				return nil, fmt.Errorf("malformed note list on %s", ref)
			}
			payload, err := gitRun(ctx, root, "notes", "--ref="+snapshotRef, "show", fields[1])
			if err != nil {
				return nil, err
			}
			author, email := "", ""
			if path := pathsByRevision[fields[1]]; path != "" {
				author, email, err = noteAuthor(ctx, root, snapshotRef, path)
				if err != nil {
					return nil, fmt.Errorf("read note author on %s for %s: %w", ref, fields[1], err)
				}
			}
			entries = append(entries, noteEntry{Ref: ref, Revision: fields[1], Payload: payload, Author: author, Email: email})
		}
	}
	return entries, nil
}

func notePaths(ctx context.Context, root, ref string) (map[string]string, error) {
	output, err := gitRun(ctx, root, "ls-tree", "-r", "--full-tree", ref)
	if err != nil {
		return nil, err
	}
	paths := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		left, path, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(left)
		if len(fields) == 3 && fields[1] == "blob" {
			revision := strings.ReplaceAll(path, "/", "")
			if !isSHA(revision) {
				continue
			}
			if previous, ok := paths[revision]; ok {
				return nil, fmt.Errorf("duplicate note paths for %s on %s: %s and %s", revision, ref, previous, path)
			}
			paths[revision] = path
		}
	}
	return paths, nil
}

func noteAuthor(ctx context.Context, root, ref, path string) (string, string, error) {
	output, err := gitRun(ctx, root, "log", "-1", "--format=%an%x00%ae", ref, "--", path)
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

func validateNoteEntry(entry noteEntry) error {
	if !json.Valid(entry.Payload) {
		return fmt.Errorf("malformed JSON note on %s for %s", entry.Ref, entry.Revision)
	}
	var err error
	switch entry.Ref {
	case "refs/notes/forest/review-request":
		_, err = decodeReview(entry.Payload, entry.Revision)
		if err == nil && !validIdentity(entry, "builder", "fixer") {
			err = fmt.Errorf("wrong author identity on review-request %s", entry.Revision)
		}
	case "refs/notes/forest/checks":
		_, err = decodeChecks(entry.Payload, entry.Revision)
		if err == nil && !validIdentity(entry, "verifier") {
			err = fmt.Errorf("wrong author identity on checks %s", entry.Revision)
		}
	case "refs/notes/forest/verdict":
		_, err = decodeVerdict(entry.Payload, entry.Revision)
		if err == nil && !validIdentity(entry, "verifier") {
			err = fmt.Errorf("wrong author identity on verdict %s", entry.Revision)
		}
	default:
		err = fmt.Errorf("unknown forest note ref %s", entry.Ref)
	}
	if err != nil {
		return fmt.Errorf("invalid note %s for %s: %v", entry.Ref, entry.Revision, err)
	}
	return nil
}

func validIdentity(entry noteEntry, roles ...string) bool {
	for _, role := range roles {
		name := "Iron Forest " + strings.ToUpper(role[:1]) + role[1:]
		if entry.Author == name && entry.Email == role+"@forest.invalid" {
			return true
		}
	}
	return false
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
		case "refs/notes/forest/review-request":
			reviewRequest = entry
			reviewRequestCount++
		case "refs/notes/forest/verdict":
			if note, err := decodeVerdict(entry.Payload, master); err == nil && note.Verdict == "approve" {
				approved = true
			}
		case "refs/notes/forest/checks":
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

func fetchAuditSnapshot(ctx context.Context, root string, acquisition auditSnapshot) (auditSnapshot, error) {
	for attempt := range auditSnapshotAttempts {
		advertised, err := advertiseAuditSnapshot(ctx, root)
		if err != nil {
			return acquisition, err
		}
		advertised.id = acquisition.id
		if err := fetchSnapshotRefs(ctx, root, advertised); err != nil {
			if cleanupErr := clearAuditSnapshot(root, acquisition); cleanupErr != nil {
				return acquisition, errors.Join(err, cleanupErr)
			}
			if attempt+1 < auditSnapshotAttempts {
				continue
			}
			return acquisition, err
		}
		observed, err := advertiseAuditSnapshot(ctx, root)
		if err != nil {
			return acquisition, err
		}
		observed.id = acquisition.id
		if sameAuditSnapshot(advertised, observed) {
			return observed, nil
		}
		if err := clearAuditSnapshot(root, acquisition); err != nil {
			return acquisition, err
		}
	}
	return acquisition, fmt.Errorf("remote snapshot changed during audit")
}

func advertiseAuditSnapshot(ctx context.Context, root string) (auditSnapshot, error) {
	args := []string{"ls-remote", "origin", "refs/heads/master"}
	args = append(args, forestNoteRefs[:]...)
	output, err := gitRun(ctx, root, args...)
	if err != nil {
		return auditSnapshot{}, fmt.Errorf("read remote snapshot: %w", err)
	}
	snapshot := auditSnapshot{Notes: make(map[string]string, len(forestNoteRefs))}
	seen := make(map[string]bool, len(forestNoteRefs)+1)
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
			known := false
			for _, canonical := range forestNoteRefs {
				if ref == canonical {
					known = true
					break
				}
			}
			if !known {
				return auditSnapshot{}, fmt.Errorf("malformed remote snapshot ref %s", ref)
			}
			snapshot.Notes[ref] = fields[0]
		}
		seen[ref] = true
	}
	if snapshot.Master == "" {
		return auditSnapshot{}, fmt.Errorf("origin/master is missing or malformed")
	}
	for _, ref := range forestNoteRefs {
		if _, ok := snapshot.Notes[ref]; !ok {
			snapshot.Notes[ref] = ""
		}
	}
	return snapshot, nil
}

func sameAuditSnapshot(left, right auditSnapshot) bool {
	if left.Master != right.Master {
		return false
	}
	for _, ref := range forestNoteRefs {
		if left.Notes[ref] != right.Notes[ref] {
			return false
		}
	}
	return true
}

func fetchSnapshotRefs(ctx context.Context, root string, snapshot auditSnapshot) error {
	if err := fetchSnapshotRef(ctx, root, "refs/heads/master", auditorMasterRef(snapshot), snapshot.Master); err != nil {
		return err
	}
	for _, ref := range forestNoteRefs {
		if err := fetchSnapshotRef(ctx, root, ref, auditorNoteRef(snapshot, ref), snapshot.Notes[ref]); err != nil {
			return err
		}
	}
	return nil
}

func fetchSnapshotRef(ctx context.Context, root, source, destination, oid string) error {
	if oid == "" {
		if _, err := gitRun(ctx, root, "update-ref", "-d", destination); err != nil {
			return fmt.Errorf("clear absent %s: %w", source, err)
		}
		return nil
	}
	if _, err := gitRun(ctx, root, "fetch", "--no-tags", "origin", source+":"+destination); err != nil {
		return fmt.Errorf("fetch %s: %w", source, err)
	}
	if err := verifyAuditRef(ctx, root, destination, oid); err != nil {
		return err
	}
	return nil
}

func verifyAuditRef(ctx context.Context, root, ref, expected string) error {
	output, err := gitRun(ctx, root, "for-each-ref", "--format=%(objectname)", ref)
	if err != nil {
		return fmt.Errorf("verify fetched %s: %w", ref, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 1 || fields[0] != expected {
		return fmt.Errorf("fetched %s does not match advertised object", ref)
	}
	return nil
}

func clearAuditSnapshot(root string, snapshot auditSnapshot) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var cleanup error
	for _, ref := range forestNoteRefs {
		privateRef := auditorNoteRef(snapshot, ref)
		if _, err := gitRun(ctx, root, "update-ref", "-d", privateRef); err != nil {
			cleanup = errors.Join(cleanup, fmt.Errorf("clear %s: %w", privateRef, err))
		}
	}
	privateMaster := auditorMasterRef(snapshot)
	if _, err := gitRun(ctx, root, "update-ref", "-d", privateMaster); err != nil {
		cleanup = errors.Join(cleanup, fmt.Errorf("clear %s: %w", privateMaster, err))
	}
	return cleanup
}

var gitRun = func(ctx context.Context, root string, args ...string) ([]byte, error) {
	path, err := trustedExecutable(root, "git")
	if err != nil {
		return nil, err
	}
	return processGroupOutput(ctx, exec.Command(path, append([]string{"-C", root}, args...)...))
}

func isSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') && !(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
