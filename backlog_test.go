package main

import "testing"

func TestSubjectSetReportsContentionAndIdle(t *testing.T) {
	set := newSubjectSet()
	if !set.idle() {
		t.Fatal("new subject set must be idle")
	}
	if !set.claim("branch-forest/7-fix") {
		t.Fatal("first subject claim must succeed")
	}
	if set.idle() {
		t.Fatal("subject set with an active key must not be idle")
	}
	if set.claim("branch-forest/7-fix") {
		t.Fatal("second subject claim must be refused")
	}
	set.release("branch-forest/7-fix")
	if !set.idle() || !set.claim("branch-forest/7-fix") {
		t.Fatal("released key must become available")
	}
	set.release("branch-forest/7-fix")
}

func TestEligibleItemsExcludesLabelsAndCoveredBranches(t *testing.T) {
	labelled := issue{Number: 2}
	labelled.Labels = append(labelled.Labels, struct {
		Name string `json:"name"`
	}{Name: "parked"})
	items := []issue{{Number: 1, Title: "ready"}, labelled, {Number: 3, Title: "covered"}}
	got := eligibleFrom(items, []string{"forest/3-covered"}, []string{"parked"}, nil)
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("eligible items = %+v, want only item 1", got)
	}
}

func TestEligibleItemsRequireLabelsOptIn(t *testing.T) {
	label := func(number int, names ...string) issue {
		it := issue{Number: number}
		for _, name := range names {
			it.Labels = append(it.Labels, struct {
				Name string `json:"name"`
			}{Name: name})
		}
		return it
	}
	unlabelled := label(1)
	promoted := label(2, "forest:ready")
	braked := label(3, "forest:failed", "forest:ready")
	items := []issue{unlabelled, promoted, braked}

	// With require_labels declared, selection is an opt-in: only an open item
	// carrying the required label is selected. ExcludeLabels composes with the
	// opt-in, so an item that also carries the excluded label is not selected.
	got := eligibleFrom(items, nil, []string{"forest:failed"}, []string{"forest:ready"})
	if len(got) != 1 || got[0].Number != 2 {
		t.Fatalf("opt-in eligible items = %+v, want only item 2", got)
	}
}
