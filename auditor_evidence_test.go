package main

import (
	"context"
	"strings"
	"testing"
)

func TestAuditorIgnoresLegacyNotes(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addRemoteNote(t, root, "refs/notes/forest/checks", sha, `{"status":"fail","results":[]}`, "phaedrus", "phraznikov@gmail.com")
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 0 {
		t.Fatalf("legacy notes produced violations=%v", result.Violations)
	}
	if result.Master != sha {
		t.Fatalf("audited master=%s want %s", result.Master, sha)
	}
}

func TestAuditorToleratesLegacyV1RequestButFlagsUnknownV2Key(t *testing.T) {
	root, master := newAdvancedAuditFixture(t, "")
	legacy := strings.Repeat("a", 40)
	unknown := strings.Repeat("b", 40)
	pushEvidence(t, root, "request", legacy,
		`{"schema":"forest.review-request.v1","issue":284,"branch":"forest/284-evidence-ref-selection","revision":"`+legacy+`","time":"2026-08-18T14:31:37Z"}`+"\n",
		"Iron Forest Builder", "builder@forest.invalid")
	pushEvidence(t, root, "request", unknown,
		`{"schema":"forest.review-request.v2","extra":true,"subject":"1","branch":"forest/1/gate","revision":"`+unknown+`","time":"2026-08-10T00:00:00Z"}`+"\n",
		"Iron Forest Builder", "builder@forest.invalid")
	addGateNotes(t, root, master, `[{"name":"test","ok":true,"exit":0}]`)

	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Violations) != 1 || !strings.Contains(result.Violations[0], "unknown JSON object key") {
		t.Fatalf("violations=%v, want exactly the unknown-key violation for the v2 payload", result.Violations)
	}
}

func TestAuditFetchesEvidenceInOneWildcardTransport(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)

	deps := defaultAuditDependencies()
	runGit := deps.runGit
	wildcardFetches := 0
	deps.runGit = func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "fetch" {
			for _, arg := range args {
				if strings.HasPrefix(arg, evidenceRefPrefix+"*:") {
					wildcardFetches++
				}
			}
		}
		return runGit(ctx, root, args...)
	}

	if _, err := auditWithDependencies(context.Background(), root, deps); err != nil {
		t.Fatal(err)
	}
	if wildcardFetches != 1 {
		t.Fatalf("wildcard evidence fetches=%d want 1", wildcardFetches)
	}
}

func TestVerifyEvidenceSnapshotRefsRejectsChangedObject(t *testing.T) {
	revision := strings.Repeat("c", 40)
	advertised := strings.Repeat("a", 40)
	fetched := strings.Repeat("b", 40)
	snapshot := auditSnapshot{
		id:       "snapshot",
		Evidence: map[string]string{evidenceRequestRefPrefix + revision: advertised},
	}
	deps := auditDependencies{runGit: func(ctx context.Context, root string, args ...string) ([]byte, error) {
		if len(args) == 0 || args[0] != "for-each-ref" {
			t.Fatalf("unexpected git command %v", args)
		}
		return []byte(fetched + "\n"), nil
	}}

	err := verifyEvidenceSnapshotRefs(context.Background(), "/repo", snapshot, deps)
	if err == nil || !strings.Contains(err.Error(), "does not match advertised object") {
		t.Fatalf("verifyEvidenceSnapshotRefs error=%v, want advertised-object mismatch", err)
	}
}
