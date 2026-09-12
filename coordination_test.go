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

func TestDecodeReviewPreservesLegacyV1Evidence(t *testing.T) {
	sha := strings.Repeat("b", 40)
	payload := `{"schema":"forest.review-request.v1","issue":4,"branch":"forest/4-work","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`
	note, err := decodeReview([]byte(payload), sha)
	if err != nil || note.Subject != "4" || note.Branch != "forest/4-work" {
		t.Fatalf("legacy evidence lost: %#v %v", note, err)
	}
}

func TestDecodeReviewRejectsCrossFields(t *testing.T) {
	sha := strings.Repeat("b", 40)
	cases := []string{
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

func TestDecodeReviewAuthority(t *testing.T) {
	sha := strings.Repeat("a", 40)
	base := `{"schema":"forest.review-request.v3","subject":"work","branch":"forest/work/implementation","revision":"` + sha + `","time":"2026-09-12T00:00:00Z","run_id":"10-builder"`
	for _, test := range []struct {
		name      string
		field     string
		authority string
		valid     bool
	}{
		{"absent", "", "", true},
		{"land", `,"authority":"land"`, "land", true},
		{"review", `,"authority":"review"`, "review", true},
		{"empty", `,"authority":""`, "", false},
		{"null", `,"authority":null`, "", false},
		{"unknown", `,"authority":"merge"`, "", false},
		{"case", `,"authority":"Review"`, "", false},
		{"number", `,"authority":1`, "", false},
		{"boolean", `,"authority":true`, "", false},
		{"duplicate", `,"authority":"review","authority":"land"`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			note, err := decodeReview([]byte(base+test.field+"}"), sha)
			if (err == nil) != test.valid {
				t.Fatalf("authority %s: note=%#v error=%v", test.field, note, err)
			}
			if test.valid && note.Authority != test.authority {
				t.Fatalf("authority=%q want %q", note.Authority, test.authority)
			}
		})
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
