package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const reservedCleanupTimeout = 30 * time.Second

func cleanupReservedResidue(root string, runner *Runner) error {
	if _, err := os.Lstat(filepath.Join(root, ".git")); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect repository metadata: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), reservedCleanupTimeout)
	defer cancel()

	err := cleanupReservedResidueWith(ctx, root, runner, os.Remove)
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		err = errors.Join(err, ctxErr)
	}
	return err
}

func cleanupReservedResidueWith(ctx context.Context, root string, runner *Runner, remove func(string) error) error {
	worktreeErr := cleanupReservedWorktrees(ctx, root, runner)
	var refErr error
	if worktreeErr == nil {
		refErr = cleanupReservedRefs(ctx, root, runner)
	}
	var liveErr error
	if worktreeErr == nil {
		liveErr = cleanupLiveRunRecords(ctx, root, remove)
	}
	return errors.Join(refErr, worktreeErr, cleanupReservedTemps(ctx, root, remove), liveErr, cleanupPiResidue(ctx, root, runner))
}

func cleanupPiResidue(ctx context.Context, root string, runner *Runner) error {
	entries, err := os.ReadDir(forestPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "pi-") || !isReservedRunID(strings.TrimPrefix(entry.Name(), "pi-")) {
			continue
		}
		if err := runner.removeFilesystem(ctx, forestPath(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func cleanupReservedWorktrees(ctx context.Context, root string, runner *Runner) error {
	var cleanupErr error
	for _, namespace := range []string{"worktrees", "checks"} {
		dir := forestPath(root, namespace)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("enumerate reserved %s: %w", namespace, err))
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || !isReservedRunID(entry.Name()) {
				continue
			}
			if err := ctx.Err(); err != nil {
				return errors.Join(cleanupErr, err)
			}
			if namespace == "worktrees" {
				record, found, findErr := FindRun(root, entry.Name())
				if findErr != nil {
					cleanupErr = errors.Join(cleanupErr, findErr)
					continue
				}
				// Unknown and unsuccessful Runs retain native Git source.
				// Check scratch has separate path ownership and is disposable.
				if !found || record.Recovery != nil || record.Exit != 0 ||
					(record.Outcome != runOutcomeCompleted && record.Outcome != runOutcomeNoWork) {
					continue
				}
			}
			if err := runner.removeWorktree(ctx, filepath.Join(dir, entry.Name()), entry.Name()); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove reserved %s %s: %w", namespace, entry.Name(), err))
			}
		}
	}
	if _, err := runner.git(ctx, root, "worktree", "prune", "--expire=now"); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("prune reserved worktree registry: %w", err))
	}
	return cleanupErr
}

func isReservedRunID(name string) bool {
	dash := strings.IndexByte(name, '-')
	if dash < 1 || dash == len(name)-1 {
		return false
	}
	for _, character := range name[:dash] {
		if character < '0' || character > '9' {
			return false
		}
	}
	for _, character := range name[dash+1:] {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func cleanupReservedRefs(ctx context.Context, root string, runner *Runner) error {
	namespaces := reservedRefNamespaces()
	args := []string{"for-each-ref", "--format=%(refname)"}
	args = append(args, namespaces[:]...)
	output, err := runner.git(ctx, root, args...)
	if err != nil {
		return fmt.Errorf("enumerate reserved refs: %w", err)
	}

	refs := strings.Fields(string(output))
	for _, ref := range refs {
		reserved := false
		for _, namespace := range namespaces {
			if strings.HasPrefix(ref, namespace) {
				reserved = true
				break
			}
		}
		if !reserved {
			return fmt.Errorf("ref outside reserved namespaces: %s", ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}

	var transaction strings.Builder
	transaction.WriteString("start\n")
	for _, ref := range refs {
		transaction.WriteString("delete ")
		transaction.WriteString(ref)
		transaction.WriteByte('\n')
	}
	transaction.WriteString("prepare\ncommit\n")
	if _, err := runner.gitInput(ctx, root, strings.NewReader(transaction.String()), "update-ref", "--no-deref", "--stdin"); err != nil {
		return fmt.Errorf("delete reserved refs: %w", err)
	}
	return nil
}

func reservedRefNamespaces() [5]string {
	return [5]string{
		pollNotesNamespace + "/",
		"refs/notes/forest-audit/",
		auditorNotesNamespace + "/",
		auditorMasterNamespace + "/",
		pollPrimaryPrivatePrefix,
	}
}

func cleanupReservedTemps(ctx context.Context, root string, remove func(string) error) error {
	dir := forestPath(root)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("enumerate reserved temps: %w", err)
	}

	var cleanupErr error
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !isReservedTemp(entry.Name()) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(cleanupErr, err)
		}
		if err := remove(filepath.Join(dir, entry.Name())); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove reserved temp %s: %w", entry.Name(), err))
		}
	}
	return cleanupErr
}

func isReservedTemp(name string) bool {
	return strings.HasPrefix(name, ".audit.json-") ||
		strings.HasPrefix(name, ".audit.log-") ||
		(strings.HasPrefix(name, "triggers.json.") && strings.HasSuffix(name, ".tmp"))
}

// cleanupLiveRunRecords removes stale per-agent live Run owner records. At
// startup no Run is in flight, so any record left behind is residue from an
// unclean termination and would otherwise be published forever as a live Run.
func cleanupLiveRunRecords(ctx context.Context, root string, remove func(string) error) error {
	dir := forestPath(root, "runs")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("enumerate live run records: %w", err)
	}

	var cleanupErr error
	for _, entry := range entries {
		if !entry.Type().IsRegular() || !isLiveRunRecord(entry.Name()) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(cleanupErr, err)
		}
		if err := remove(filepath.Join(dir, entry.Name())); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove live run record %s: %w", entry.Name(), err))
		}
	}
	return cleanupErr
}

func isLiveRunRecord(name string) bool {
	return strings.HasPrefix(name, "live-") && strings.HasSuffix(name, ".json")
}
