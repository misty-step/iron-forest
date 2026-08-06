package main

import (
	"fmt"
	"testing"
)

// stubGH replaces the gh CLI with a canned responder and restores it when the
// test finishes, so backlog tests run offline.
func stubGH(t *testing.T, respond func(args []string) []byte) {
	t.Helper()
	orig := ghJSON
	ghJSON = func(args ...string) ([]byte, error) {
		out := respond(args)
		if out == nil {
			return nil, fmt.Errorf("unexpected gh args: %v", args)
		}
		return out, nil
	}
	t.Cleanup(func() { ghJSON = orig })
}

// TestBacklogFiltersClaimedFailedAndParked pins the label filter: an issue
// carrying any forest lifecycle label must never re-enter the backlog.
func TestBacklogFiltersLifecycleLabels(t *testing.T) {
	stubGH(t, func(args []string) []byte {
		switch args[0] {
		case "issue":
			return []byte(`[
				{"number":1,"title":"claimed","labels":[{"name":"forest:wip"}]},
				{"number":2,"title":"failed","labels":[{"name":"forest:failed"}]},
				{"number":3,"title":"parked item","labels":[{"name":"parked"}]},
				{"number":4,"title":"ready","labels":[]}
			]`)
		case "pr":
			return []byte(`[]`)
		}
		return nil
	})
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"

	ready, err := backlog(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].Number != 4 {
		t.Fatalf("backlog = %+v, want only issue 4", ready)
	}
}

// TestBacklogDropsIssueWithOpenPR pins the dedupe filter: an already-covered
// issue is not reshaped when an open PR references it.
func TestBacklogDropsIssueWithOpenPR(t *testing.T) {
	stubGH(t, func(args []string) []byte {
		switch args[0] {
		case "issue":
			return []byte(`[
				{"number":4,"title":"ready","labels":[]},
				{"number":5,"title":"in flight","labels":[]}
			]`)
		case "pr":
			// One open PR references #5, none reference #4.
			return []byte(`[{"number":100,"title":"","body":"fixes #5","url":"http://e/100","headRefName":"b"}]`)
		}
		return nil
	})
	cfg := defaultConfig()
	cfg.Repo = "owner/repo"

	ready, err := backlog(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].Number != 4 {
		t.Fatalf("backlog = %+v, want only issue 4", ready)
	}
}
