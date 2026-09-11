package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// RunRecovery points to native Git source custody, not a resumable Pi session.
// Availability ends only when an operator disposes of the worktree.
type RunRecovery struct {
	Path         string `json:"path"`
	BaseRevision string `json:"base_revision,omitempty"`
}

func (r *Runner) retainedWorktree(ctx context.Context, runID string) *RunRecovery {
	if !isReservedRunID(runID) {
		return nil
	}
	path := forestPath(r.Root, "worktrees", runID)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	recovery := &RunRecovery{Path: filepath.Join(workspaceName, "worktrees", runID)}
	if base, err := r.git(ctx, path, "rev-parse", "--verify", "HEAD"); err == nil {
		recovery.BaseRevision = strings.TrimSpace(string(base))
	}
	return recovery
}
