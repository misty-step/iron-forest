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
	Ref      string
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
	if err := clearPersistedAuditErrors(root); err != nil {
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
		Evidence: map[string]string{},
		id:       fmt.Sprintf("%x", id[:]),
	}, nil
}

func auditorMasterRef(snapshot auditSnapshot) string {
	return auditorMasterNamespace + "/" + snapshot.id + "/master"
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

func runAuditGit(ctx context.Context, root string, args ...string) ([]byte, error) {
	path, err := trustedExecutable(root, "git")
	if err != nil {
		return nil, err
	}
	return processGroupOutput(ctx, exec.Command(path, append([]string{"-C", root}, args...)...))
}
