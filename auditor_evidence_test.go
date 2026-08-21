package main

import (
	"context"
	"strings"
	"testing"
)

func TestAuditorIgnoresLegacyNotes(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addRemoteNote(t, root, checksNoteRef, sha, `{"status":"fail","results":[]}`, "phaedrus", "phraznikov@gmail.com")
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
