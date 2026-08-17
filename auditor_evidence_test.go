package main

import (
	"context"
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
