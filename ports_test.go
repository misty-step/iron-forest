package main

import "testing"

// TestTrackerFake drives the in-memory tracker through the Tracker interface.
func TestTrackerFake(t *testing.T) {
	store := newMemoryTracker()
	if _, err := store.Get("7"); err == nil {
		t.Fatal("Get of a missing item must fail")
	}
	store.seed(Item{ID: "7", Title: "build the parser", Tags: []string{"forest:ready"}})

	var src Tracker = store
	if items, err := src.ListOpen(); err != nil || len(items) != 1 || items[0].ID != "7" {
		t.Fatalf("ListOpen = %+v, %v; want one item id 7", items, err)
	}
	if got, err := src.Get("7"); err != nil || got.Title != "build the parser" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if err := src.Comment("7", "let's ship"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if err := src.SetTags("7", []string{"forest:reviewed"}, []string{"forest:ready"}); err != nil {
		t.Fatalf("SetTags: %v", err)
	}

	got, err := src.Get("7")
	if err != nil {
		t.Fatalf("Get after writes: %v", err)
	}
	if len(got.Comments) != 1 || got.Comments[0].Body != "let's ship" {
		t.Fatalf("Comments = %+v", got.Comments)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "forest:reviewed" {
		t.Fatalf("Tags = %v, want [forest:reviewed]", got.Tags)
	}

	if err := src.Close("7"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if items, err := src.ListOpen(); err != nil || len(items) != 0 {
		t.Fatalf("ListOpen after Close = %+v, %v; want empty", items, err)
	}
}

// TestProjectionFake drives the in-memory projection through the Projection
// interface.
func TestProjectionFake(t *testing.T) {
	proj := newMemoryProjection()
	if _, err := proj.FindOpen("forest/7-change"); err == nil {
		t.Fatal("FindOpen of a missing branch must fail")
	}

	var dst Projection = proj
	opened, err := dst.Open("forest/7-change", "forest: build the parser", "body")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if opened.ID == "" {
		t.Fatal("an opened projection must carry an id")
	}
	if found, err := dst.FindOpen("forest/7-change"); err != nil || found.ID != opened.ID {
		t.Fatalf("FindOpen = %+v, %v; want id %q", found, err, opened.ID)
	}
	if err := dst.Comment(opened.ID, "verdict: approve"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if err := dst.Merge(opened.ID, "squash"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if _, err := dst.FindOpen("forest/7-change"); err == nil {
		t.Fatal("FindOpen after Merge must fail")
	}
}
