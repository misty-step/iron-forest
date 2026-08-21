package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestAuditorRejectsChecksAndApproveWithoutReviewRequest(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	state, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	addVerifierGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !containsViolation(result.Violations, "exactly one valid review-request note") {
		t.Fatalf("checks and approve passed without a review request: %v", result.Violations)
	}
	current, err := readAuditState(root)
	if err != nil {
		t.Fatal(err)
	}
	if current.LastMaster != state.LastMaster {
		t.Fatalf("master without a review request became last-good: before=%#v after=%#v", state, current)
	}
}

func TestAuditorRequiresExactlyOneValidReviewRequest(t *testing.T) {
	master := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	validPayload := `{"schema":"forest.review-request.v2","subject":"1","branch":"forest/1/gate","revision":"` + master + `","time":"2026-08-10T00:00:00Z"}`
	validRequest := noteEntry{
		Ref:      evidenceRequestRefPrefix + master,
		Revision: master,
		Payload:  []byte(validPayload),
		Author:   "Iron Forest Builder",
		Email:    "builder@forest.invalid",
	}
	fixerRequest := validRequest
	fixerRequest.Author = "Iron Forest Fixer"
	fixerRequest.Email = "fixer@forest.invalid"
	verifierEvidence := []noteEntry{
		{
			Ref:      evidenceChecksRefPrefix + master,
			Revision: master,
			Payload:  []byte(`{"schema":"forest.checks.v1","revision":"` + master + `","results":[{"name":"test","ok":true,"exit":0}],"time":"2026-08-10T00:00:00Z"}`),
			Author:   "Iron Forest Verifier",
			Email:    "verifier@forest.invalid",
		},
		{
			Ref:      evidenceVerdictRefPrefix + master,
			Revision: master,
			Payload:  []byte(`{"schema":"forest.verdict.v1","revision":"` + master + `","verdict":"approve","summary":"ok","time":"2026-08-10T00:00:00Z"}`),
			Author:   "Iron Forest Verifier",
			Email:    "verifier@forest.invalid",
		},
	}
	malformed := validRequest
	malformed.Payload = []byte("garbage")
	wrongTarget := validRequest
	wrongTarget.Revision = other
	wrongTarget.Ref = evidenceRequestRefPrefix + other
	wrongTarget.Payload = []byte(`{"schema":"forest.review-request.v2","subject":"1","branch":"forest/1/gate","revision":"` + other + `","time":"2026-08-10T00:00:00Z"}`)
	wrongIdentity := validRequest
	wrongIdentity.Author = "Unexpected"
	wrongIdentity.Email = "unexpected@forest.invalid"
	wrongBranch := validRequest
	wrongBranch.Payload = []byte(`{"schema":"forest.review-request.v2","subject":"1","branch":"forest/2/wrong","revision":"` + master + `","time":"2026-08-10T00:00:00Z"}`)
	forbiddenSpace := validRequest
	forbiddenSpace.Payload = []byte(`{"schema":"forest.review-request.v2","subject":"1","branch":"forest/1/bad branch","revision":"` + master + `","time":"2026-08-10T00:00:00Z"}`)
	forbiddenDots := validRequest
	forbiddenDots.Payload = []byte(`{"schema":"forest.review-request.v2","subject":"1","branch":"forest/1/bad..branch","revision":"` + master + `","time":"2026-08-10T00:00:00Z"}`)
	cases := []struct {
		name     string
		requests []noteEntry
		valid    bool
	}{
		{name: "missing"},
		{name: "malformed", requests: []noteEntry{malformed}},
		{name: "wrong target", requests: []noteEntry{wrongTarget}},
		{name: "wrong identity", requests: []noteEntry{wrongIdentity}},
		{name: "wrong branch", requests: []noteEntry{wrongBranch}},
		{name: "forbidden branch space", requests: []noteEntry{forbiddenSpace}},
		{name: "forbidden branch dots", requests: []noteEntry{forbiddenDots}},
		{name: "duplicate", requests: []noteEntry{validRequest, fixerRequest}},
		{name: "duplicate with malformed", requests: []noteEntry{validRequest, malformed}},
		{name: "single fixer", requests: []noteEntry{fixerRequest}, valid: true},
		{name: "single powder v2", requests: []noteEntry{{
			Ref:      evidenceRequestRefPrefix + master,
			Revision: master,
			Payload:  []byte(`{"schema":"forest.review-request.v2","subject":"iron-forest-ready","branch":"forest/iron-forest-ready/work","revision":"` + master + `","time":"2026-08-10T00:00:00Z"}`),
			Author:   "Iron Forest Builder",
			Email:    "builder@forest.invalid",
		}}, valid: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			entries := slices.Clone(testCase.requests)
			entries = append(entries, verifierEvidence...)
			err := verifyEvidenceGate(entries, master, Config{Checks: []Check{{Name: "test"}}})
			if testCase.valid && err != nil {
				t.Fatalf("verifyEvidenceGate rejected valid Fixer request: %v", err)
			}
			if !testCase.valid && (err == nil || !strings.Contains(err.Error(), "exactly one valid review-request note")) {
				t.Fatalf("verifyEvidenceGate error=%v", err)
			}
		})
	}
}

func TestAuditorRejectsChecksSetMismatch(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		results string
		want    string
	}{
		{name: "empty reported", config: "", results: `[]`, want: "invalid checks note"},
		{name: "partial", config: "repo: owner/name\nprimary: refs/heads/master\nagents:\n  builder: {poll: forest poll builder, interval: 1}\nchecks:\n  - {name: test, run: test}\n  - {name: vet, run: vet}\n", results: `[{"name":"test","ok":true,"exit":0}]`, want: "does not match configured checks"},
		{name: "extra", config: "", results: `[{"name":"test","ok":true,"exit":0},{"name":"vet","ok":true,"exit":0}]`, want: "does not match configured checks"},
		{name: "order mismatch", config: "repo: owner/name\nprimary: refs/heads/master\nagents:\n  builder: {poll: forest poll builder, interval: 1}\nchecks:\n  - {name: test, run: test}\n  - {name: vet, run: vet}\n", results: `[{"name":"vet","ok":true,"exit":0},{"name":"test","ok":true,"exit":0}]`, want: "does not match configured checks"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			root, sha := newAdvancedAuditFixture(t, testCase.config)
			addGateNotes(t, root, sha, testCase.results)
			result, err := audit(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !containsViolation(result.Violations, testCase.want) {
				t.Fatalf("violations=%v", result.Violations)
			}
		})
	}
}

func TestAuditorAcceptsAdvancedFastForwardGate(t *testing.T) {
	root, sha := newAdvancedAuditFixture(t, "")
	addGateNotes(t, root, sha, `[{"name":"test","ok":true,"exit":0}]`)
	result, err := audit(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Advanced || len(result.Violations) != 0 {
		t.Fatalf("advanced gate result=%#v", result)
	}
}
