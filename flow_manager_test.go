package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func managerCfg() ManagerFlowCfg {
	return ManagerFlowCfg{
		FlowCfg:      FlowCfg{Enabled: true, Agent: "manager", IntervalSec: 120},
		MaxOpenReady: 1,
		PromoteTag:   "forest:ready",
		ExcludeTags:  []string{failedLabel, "parked"},
	}
}

// TestManagerPromotesAtMostTheOpenLevel proves the level cap is the primary
// control: with several shaped, unlabeled, unblocked items and max_open_ready 1,
// one pass promotes at most one of them.
func TestManagerPromotesAtMostTheOpenLevel(t *testing.T) {
	tk := newMemoryTracker()
	items := []Item{
		{ID: "1", Title: "alpha", UpdatedAt: "r1", Body: "clear scope"},
		{ID: "2", Title: "beta", UpdatedAt: "r2", Body: "clear scope"},
		{ID: "3", Title: "gamma", UpdatedAt: "r3", Body: "clear scope"},
	}
	for _, it := range items {
		tk.seed(it)
	}
	rep := managerReport{
		Promote: []string{"1", "2", "3"},
	}
	promoted, err := applyManager(managerCfg(), tk, items, nil, rep)
	if err != nil {
		t.Fatal(err)
	}
	if promoted != 1 {
		t.Fatalf("one pass promoted %d, want 1 (max_open_ready)", promoted)
	}
	got := tk.items["1"]
	if !got.hasTag("forest:ready") {
		t.Fatal("item 1 should carry the promote tag")
	}
	for _, id := range []string{"2", "3"} {
		if tk.items[id].hasTag("forest:ready") {
			t.Fatalf("item %s promoted beyond the open level", id)
		}
	}
}

// TestManagerPromotesNothingAtOpenLevel proves that once max_open_ready items
// are already promoted and unbranched, a pass promotes nothing.
func TestManagerPromotesNothingAtOpenLevel(t *testing.T) {
	tk := newMemoryTracker()
	items := []Item{
		{ID: "1", Title: "in flight", UpdatedAt: "r1", Tags: []string{"forest:ready"}},
		{ID: "2", Title: "next", UpdatedAt: "r2", Body: "clear scope"},
	}
	for _, it := range items {
		tk.seed(it)
	}
	rep := managerReport{Promote: []string{"2"}}
	promoted, err := applyManager(managerCfg(), tk, items, nil, rep)
	if err != nil {
		t.Fatal(err)
	}
	if promoted != 0 {
		t.Fatalf("promoted %d, want 0 with the open level already full", promoted)
	}
	if tk.items["2"].hasTag("forest:ready") {
		t.Fatal("item 2 must not be promoted while the open level is full")
	}
}

// TestManagerNeverPromotesOpenBlocker proves an item whose `Blocked by:`
// reference is still open is never promoted, and the recorded reason names the
// blocker.
func TestManagerNeverPromotesOpenBlocker(t *testing.T) {
	tk := newMemoryTracker()
	items := []Item{
		{ID: "149", Title: "still open", UpdatedAt: "r1"},
		{ID: "70", Title: "waiting", UpdatedAt: "r2", Body: "Blocked by: #149"},
	}
	for _, it := range items {
		tk.seed(it)
	}
	rep := managerReport{
		Promote: []string{"70"},
		Reject:  []managerReject{{ID: "70"}},
	}
	promoted, err := applyManager(managerCfg(), tk, items, nil, rep)
	if err != nil {
		t.Fatal(err)
	}
	if promoted != 0 {
		t.Fatalf("promoted a blocked item; want 0")
	}
	if tk.items["70"].hasTag("forest:ready") {
		t.Fatal("blocked item must not carry the promote tag")
	}
	comments := tk.items["70"].Comments
	if len(comments) != 1 {
		t.Fatalf("blocked item got %d comments, want 1", len(comments))
	}
	if !strings.Contains(comments[0].Body, "#149") {
		t.Fatalf("the recorded reason must name the blocker, got %q", comments[0].Body)
	}
}

// TestManagerCommentIdempotentAtUnchangedRevision proves repeated passes at the
// same revision add no second comment: the lane scans its own marker before
// commenting, so a second pass over the same static item stays silent.
func TestManagerCommentIdempotentAtUnchangedRevision(t *testing.T) {
	tk := newMemoryTracker()
	items := []Item{
		{ID: "9", Title: "unshaped", UpdatedAt: "r9", Body: "vague"},
	}
	tk.seed(items[0])
	rep := managerReport{Reject: []managerReject{{ID: "9", Reason: "no definition of done"}}}

	if _, err := applyManager(managerCfg(), tk, items, nil, rep); err != nil {
		t.Fatal(err)
	}
	if len(tk.items["9"].Comments) != 1 {
		t.Fatalf("first pass wrote %d comments, want 1", len(tk.items["9"].Comments))
	}
	// A second pass at the same revision sees the item with its comment thread
	// (a real pass re-lists from the tracker), so the marker scan stays silent.
	fresh, err := tk.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyManager(managerCfg(), tk, fresh, nil, rep); err != nil {
		t.Fatal(err)
	}
	if len(tk.items["9"].Comments) != 1 {
		t.Fatalf("second pass wrote %d comments, want still 1", len(tk.items["9"].Comments))
	}
}

// TestManagerRejectsUnselectedReport proves a report that names an item the
// lane did not offer is rejected at the gate.
func TestManagerRejectsUnselectedReport(t *testing.T) {
	wtDir := t.TempDir()
	cands := []Item{{ID: "1", Title: "offered", UpdatedAt: "r1"}}
	if err := os.WriteFile(filepath.Join(wtDir, "report.json"),
		[]byte(`{"promote":["1"],"reject":[{"id":"42","reason":"never offered"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gateManagerReport(wtDir, cands); err == nil {
		t.Fatal("report naming an unselected item must be rejected")
	}
}

// TestManagerRejectsNullAndShapelessReport proves the gate rejects the exact
// schema-invalid shapes the reviewer called out: a null promote array and a
// reject entry that omits its nested id or reason. An empty array is still
// legitimately accepted (a pass may have nothing to promote).
func TestManagerRejectsNullAndShapelessReport(t *testing.T) {
	cands := []Item{{ID: "1", Title: "offered", UpdatedAt: "r1"}}
	cases := []struct {
		name string
		body string
	}{
		{"null promote", `{"promote":null,"reject":[]}`},
		{"null reject", `{"promote":[],"reject":null}`},
		{"reject without id", `{"promote":[],"reject":[{"reason":"shapeless"}]}`},
		{"reject without reason", `{"promote":[],"reject":[{"id":"1"}]}`},
		{"reject as bare object", `{"promote":[],"reject":[{}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wtDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(wtDir, "report.json"), []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := gateManagerReport(wtDir, cands); err == nil {
				t.Fatalf("report %s must be rejected by the gate", tc.body)
			}
		})
	}
	// An empty-array report is valid: nothing was ready and nothing refused.
	wtDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wtDir, "report.json"), []byte(`{"promote":[],"reject":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gateManagerReport(wtDir, cands); err != nil {
		t.Fatalf("valid empty-array report rejected: %v", err)
	}
}

// TestManagerPromoteBlockerWritesExplanation proves a schema-valid promote entry
// whose item has an open blocker is not only skipped: the lane records the
// blocker in a comment and stays idempotent on a repeat pass, rather than
// silently dropping the entry and leaving no explanation.
func TestManagerPromoteBlockerWritesExplanation(t *testing.T) {
	tk := newMemoryTracker()
	items := []Item{
		{ID: "149", Title: "still open", UpdatedAt: "r1"},
		{ID: "70", Title: "waiting", UpdatedAt: "r2", Body: "Blocked by: #149"},
	}
	for _, it := range items {
		tk.seed(it)
	}
	// The agent promotes 70 even though it is blocked; only the blocker comment
	// should be recorded, with no promote tag applied.
	rep := managerReport{Promote: []string{"70"}}
	if _, err := applyManager(managerCfg(), tk, items, nil, rep); err != nil {
		t.Fatal(err)
	}
	if tk.items["70"].hasTag("forest:ready") {
		t.Fatal("blocked item must not carry the promote tag")
	}
	comments := tk.items["70"].Comments
	if len(comments) != 1 {
		t.Fatalf("blocked item got %d comments, want 1", len(comments))
	}
	if !strings.Contains(comments[0].Body, "#149") {
		t.Fatalf("the recorded reason must name the blocker, got %q", comments[0].Body)
	}
	// Repeat the pass at the same revision: a real pass re-lists from the
	// tracker, so the marker scan keeps it silent.
	fresh, err := tk.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyManager(managerCfg(), tk, fresh, nil, rep); err != nil {
		t.Fatal(err)
	}
	if len(tk.items["70"].Comments) != 1 {
		t.Fatalf("repeat pass wrote %d comments, want still 1", len(tk.items["70"].Comments))
	}
}

// TestManagerNeverPromotesExcludedTag proves an item carrying an excluded tag
// is never promoted even when the report names it.
func TestManagerNeverPromotesExcludedTag(t *testing.T) {
	tk := newMemoryTracker()
	items := []Item{
		{ID: "7", Title: "failed work", UpdatedAt: "r7", Tags: []string{failedLabel}},
	}
	tk.seed(items[0])
	rep := managerReport{Promote: []string{"7"}}
	promoted, err := applyManager(managerCfg(), tk, items, nil, rep)
	if err != nil {
		t.Fatal(err)
	}
	if promoted != 0 {
		t.Fatalf("promoted an excluded item; want 0")
	}
	if tk.items["7"].hasTag("forest:ready") {
		t.Fatal("excluded item must not carry the promote tag")
	}
}
