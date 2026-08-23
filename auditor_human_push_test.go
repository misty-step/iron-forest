package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuditorAcceptsHumanDirectPush(t *testing.T) {
	root, _ := testClone(t)
	baseline := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	if _, err := audit(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "human"), []byte("human\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "human")
	runGitDir(t, root, "-c", "user.name=Operator", "-c", "user.email=operator@example.com", "commit", "-m", "operator direct push")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))

	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Advanced || result.Master != tip {
		t.Fatalf("human direct push result=%#v", result)
	}
	if len(result.Violations) != 0 {
		t.Fatalf("human direct push violations=%v", result.Violations)
	}
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Baseline != baseline || state.LastMaster != tip || state.LastResult != "pass" {
		t.Fatalf("human direct push state=%#v", state)
	}
}

func TestAuditorStillRejectsAgentAuthoredDirectPush(t *testing.T) {
	agents := []struct {
		name  string
		email string
	}{
		{name: "Iron Forest Builder", email: "builder@forest.invalid"},
		{name: "Iron Forest Fixer", email: "fixer@forest.invalid"},
		{name: "Iron Forest Verifier", email: "verifier@forest.invalid"},
	}
	for _, agent := range agents {
		t.Run(agent.name, func(t *testing.T) {
			root, _ := testClone(t)
			if _, err := audit(context.Background(), root); err != nil {
				t.Fatal(err)
			}

			if err := os.WriteFile(filepath.Join(root, "agent"), []byte("agent\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runGitDir(t, root, "add", "agent")
			runGitDir(t, root, "-c", "user.name="+agent.name, "-c", "user.email="+agent.email, "commit", "-m", "agent direct push")
			runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")

			result, err := audit(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !containsViolation(result.Violations, "exactly one valid review-request note") {
				t.Fatalf("agent direct push violations=%v", result.Violations)
			}
		})
	}
}

func TestAuditorRejectsHumanPushCarryingFactoryEvidence(t *testing.T) {
	root, _ := testClone(t)
	if _, err := audit(context.Background(), root); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "human"), []byte("human\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDir(t, root, "add", "human")
	runGitDir(t, root, "-c", "user.name=Operator", "-c", "user.email=operator@example.com", "commit", "-m", "operator push with partial evidence")
	runGitDir(t, root, "push", "origin", "HEAD:refs/heads/master")
	tip := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))

	addVerifierGateNotes(t, root, tip, `[{"name":"test","ok":true,"exit":0}]`)
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "exactly one valid review-request note") {
		t.Fatalf("human push with factory evidence violations=%v", result.Violations)
	}
}
