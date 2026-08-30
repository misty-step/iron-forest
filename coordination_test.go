package main

import (
	"strings"
	"testing"
)

func TestDecodeReviewAcceptsV2(t *testing.T) {
	sha := strings.Repeat("a", 40)
	v2 := `{"schema":"forest.review-request.v2","subject":"iron-forest-ready","branch":"forest/iron-forest-ready/work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`
	note, err := decodeReview([]byte(v2), sha)
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if note.Schema != "forest.review-request.v2" || note.Subject != "iron-forest-ready" || note.Tracker != "" {
		t.Fatalf("v2 note=%#v", note)
	}
	issue := `{"schema":"forest.review-request.v2","subject":"4","branch":"forest/4/work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z","tracker":"github"}`
	note, err = decodeReview([]byte(issue), sha)
	if err != nil {
		t.Fatalf("issue subject: %v", err)
	}
	if note.Subject != "4" || note.Tracker != "github" {
		t.Fatalf("issue subject=%#v", note)
	}
	powder := `{"schema":"forest.review-request.v2","subject":"if-ready","branch":"forest/if-ready/work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z","tracker":"powder"}`
	note, err = decodeReview([]byte(powder), sha)
	if err != nil {
		t.Fatalf("powder tracker: %v", err)
	}
	if note.Tracker != "powder" {
		t.Fatalf("powder note=%#v", note)
	}
}

func TestDecodeReviewRejectsV1AndCrossFields(t *testing.T) {
	sha := strings.Repeat("b", 40)
	cases := []string{
		`{"schema":"forest.review-request.v1","issue":4,"branch":"forest/4-work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`,
		`{"schema":"forest.review-request.v2","issue":4,"subject":"4","branch":"forest/4/work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`,
		`{"schema":"forest.review-request.v2","subject":"iron-forest-ready","branch":"forest/iron-forest-ready-work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`,
		`{"schema":"forest.review-request.v2","subject":"bad id","branch":"forest/bad-id/work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`,
		`{"schema":"forest.review-request.v3","subject":"x","branch":"forest/x/work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`,
		`{"schema":"forest.review-request.v2","subject":"4","branch":"forest/4/work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z","tracker":"jira"}`,
	}
	for _, payload := range cases {
		if _, err := decodeReview([]byte(payload), sha); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}

func TestBranchGrammars(t *testing.T) {
	if validForestBranch("forest/4-work") {
		t.Fatal("hyphen grammar still accepted")
	}
	if !validForestBranch("forest/4/work") || !validForestBranch("forest/iron-forest-ready/work") {
		t.Fatal("slash grammar rejected")
	}
	if !branchBelongsToSubject("forest/4/work", "4") {
		t.Fatal("subject 4 missed forest/4/work")
	}
	if branchBelongsToSubject("forest/4/work/nested", "4") {
		t.Fatal("nested slug matched")
	}
	if branchBelongsToSubject("forest/4-work", "4") {
		t.Fatal("hyphen branch matched subject")
	}
	if validSubject("bad id") || validSubject("") || !validSubject("iron-forest-ready") || !validSubject("4") {
		t.Fatal("validSubject charset")
	}
}
