package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

func fetchEvidenceAuditSnapshot(ctx context.Context, root string, acquisition auditSnapshot, deps auditDependencies) (auditSnapshot, error) {
	for attempt := range auditSnapshotAttempts {
		advertised, err := advertiseEvidenceSnapshot(ctx, root, deps)
		if err != nil {
			return acquisition, err
		}
		advertised.id = acquisition.id
		if err := fetchEvidenceSnapshotRefs(ctx, root, advertised, deps); err != nil {
			if cleanupErr := clearEvidenceSnapshot(root, acquisition, deps); cleanupErr != nil {
				return acquisition, errors.Join(err, cleanupErr)
			}
			if attempt+1 < auditSnapshotAttempts {
				continue
			}
			return acquisition, err
		}
		observed, err := advertiseEvidenceSnapshot(ctx, root, deps)
		if err != nil {
			return acquisition, err
		}
		observed.id = acquisition.id
		if sameEvidenceSnapshot(advertised, observed) {
			return observed, nil
		}
		if err := clearEvidenceSnapshot(root, acquisition, deps); err != nil {
			return acquisition, err
		}
	}
	return acquisition, fmt.Errorf("remote snapshot changed during audit")
}

func sameEvidenceSnapshot(left, right auditSnapshot) bool {
	if left.Master != right.Master || len(left.Evidence) != len(right.Evidence) {
		return false
	}
	for ref, oid := range left.Evidence {
		if right.Evidence[ref] != oid {
			return false
		}
	}
	return true
}

func auditorEvidenceRef(snapshot auditSnapshot, ref string) string {
	return auditorNotesNamespace + "/" + snapshot.id + "/" + strings.TrimPrefix(ref, "refs/forest/")
}

func advertiseEvidenceSnapshot(ctx context.Context, root string, deps auditDependencies) (auditSnapshot, error) {
	output, err := deps.runGit(ctx, root, "ls-remote", "origin", "refs/heads/master",
		evidenceRequestRefPrefix+"*", evidenceChecksRefPrefix+"*", evidenceVerdictRefPrefix+"*")
	if err != nil {
		return auditSnapshot{}, fmt.Errorf("read remote snapshot: %w", err)
	}
	snapshot := auditSnapshot{Notes: map[string]string{}, Evidence: map[string]string{}}
	seen := map[string]bool{}
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
		} else if strings.HasPrefix(ref, "refs/forest/v1/") {
			snapshot.Evidence[ref] = fields[0]
		} else {
			return auditSnapshot{}, fmt.Errorf("malformed remote snapshot ref %s", ref)
		}
		seen[ref] = true
	}
	if snapshot.Master == "" {
		return auditSnapshot{}, fmt.Errorf("origin/master is missing or malformed")
	}
	return snapshot, nil
}

func fetchEvidenceSnapshotRefs(ctx context.Context, root string, snapshot auditSnapshot, deps auditDependencies) error {
	if err := fetchSnapshotRef(ctx, root, "refs/heads/master", auditorMasterRef(snapshot), snapshot.Master, deps); err != nil {
		return err
	}
	for ref, oid := range snapshot.Evidence {
		if err := fetchSnapshotRef(ctx, root, ref, auditorEvidenceRef(snapshot, ref), oid, deps); err != nil {
			return err
		}
	}
	return nil
}

func clearEvidenceSnapshot(root string, snapshot auditSnapshot, deps auditDependencies) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var cleanup error
	for ref := range snapshot.Evidence {
		privateRef := auditorEvidenceRef(snapshot, ref)
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

func readEvidence(ctx context.Context, root string, snapshot auditSnapshot, deps auditDependencies) ([]noteEntry, []string, error) {
	var entries []noteEntry
	var violations []string
	for ref, oid := range snapshot.Evidence {
		if oid == "" {
			continue
		}
		private := auditorEvidenceRef(snapshot, ref)
		kind, sha, ok := parseEvidenceRef(ref)
		if !ok {
			violations = append(violations, "malformed evidence ref "+ref)
			continue
		}
		payload, err := deps.runGit(ctx, root, "show", private+":"+evidenceFileName(kind))
		if err != nil {
			violations = append(violations, "missing evidence payload on "+ref)
			continue
		}
		identityOut, err := deps.runGit(ctx, root, "log", "-1", "--format=%an%x00%ae", private)
		if err != nil {
			return nil, nil, err
		}
		line, err := exactGitLine(identityOut)
		if err != nil {
			violations = append(violations, "malformed evidence identity on "+ref)
			continue
		}
		parts := strings.SplitN(line, "\x00", 2)
		if len(parts) != 2 {
			violations = append(violations, "malformed evidence identity on "+ref)
			continue
		}
		entry := noteEntry{Ref: ref, Revision: sha, Payload: payload, Author: parts[0], Email: parts[1]}
		if err := validateEvidenceEntry(entry, kind); err != nil {
			violations = append(violations, err.Error())
			continue
		}
		entries = append(entries, entry)
	}
	return entries, violations, nil
}

func parseEvidenceRef(ref string) (kind, sha string, ok bool) {
	switch {
	case strings.HasPrefix(ref, evidenceRequestRefPrefix):
		kind, sha = "request", strings.TrimPrefix(ref, evidenceRequestRefPrefix)
	case strings.HasPrefix(ref, evidenceChecksRefPrefix):
		kind, sha = "checks", strings.TrimPrefix(ref, evidenceChecksRefPrefix)
	case strings.HasPrefix(ref, evidenceVerdictRefPrefix):
		kind, sha = "verdict", strings.TrimPrefix(ref, evidenceVerdictRefPrefix)
	default:
		return "", "", false
	}
	if !isSHA(sha) {
		return "", "", false
	}
	return kind, sha, true
}

func validateEvidenceEntry(entry noteEntry, kind string) error {
	switch kind {
	case "request":
		if _, err := decodeReview(entry.Payload, entry.Revision); err != nil {
			return fmt.Errorf("invalid evidence %s: %v", entry.Ref, err)
		}
		if !validIdentity(entry, "builder", "fixer") {
			return fmt.Errorf("wrong author identity on request %s", entry.Revision)
		}
	case "checks":
		if _, err := decodeChecks(entry.Payload, entry.Revision); err != nil {
			return fmt.Errorf("invalid evidence %s: %v", entry.Ref, err)
		}
		if !validIdentity(entry, "verifier") {
			return fmt.Errorf("wrong author identity on checks %s", entry.Revision)
		}
	case "verdict":
		if _, err := decodeVerdict(entry.Payload, entry.Revision); err != nil {
			return fmt.Errorf("invalid evidence %s: %v", entry.Ref, err)
		}
		if !validIdentity(entry, "verifier") {
			return fmt.Errorf("wrong author identity on verdict %s", entry.Revision)
		}
	default:
		return fmt.Errorf("unknown evidence ref %s", entry.Ref)
	}
	return nil
}

func verifyEvidenceGate(entries []noteEntry, master string, cfg Config) error {
	var request noteEntry
	var requestCount int
	var approved bool
	var checksNotes []checksNote
	for _, entry := range entries {
		if entry.Revision != master {
			continue
		}
		kind, _, ok := parseEvidenceRef(entry.Ref)
		if !ok {
			continue
		}
		switch kind {
		case "request":
			request = entry
			requestCount++
		case "verdict":
			if note, err := decodeVerdict(entry.Payload, master); err == nil && note.Verdict == "approve" {
				approved = true
			}
		case "checks":
			if note, err := decodeChecks(entry.Payload, master); err == nil {
				checksNotes = append(checksNotes, note)
			}
		}
	}
	if requestCount != 1 {
		return fmt.Errorf("master %s does not have exactly one valid review-request note", master)
	}
	if _, err := decodeReview(request.Payload, master); err != nil || !validIdentity(request, "builder", "fixer") {
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
		if result.Name != check.Name || !result.OK || result.Exit != 0 {
			if result.Name != check.Name {
				return fmt.Errorf("master %s checks note does not match configured checks", master)
			}
			return fmt.Errorf("master %s has no passing checks note", master)
		}
	}
	return nil
}
