package main

import (
	"context"
	"os"
	"testing"
)

func TestAuditorGitRunStopsDescendants(t *testing.T) {
	testGitTransportStopsDescendants(t, "Audit", "audit-output", func(ctx context.Context, root string) ([]byte, error) {
		return runAuditGit(ctx, root, "--version")
	})
}

func TestAuditorSurfacesRemoteFailure(t *testing.T) {
	root, _ := testClone(t)
	runGitDir(t, root, "remote", "remove", "origin")
	if _, err := audit(context.Background(), root); err == nil {
		t.Fatal("expected remote audit failure")
	}
	if _, err := os.Stat(auditStatePath(root)); !os.IsNotExist(err) {
		t.Fatalf("audit state should not be written after remote failure: %v", err)
	}
}
