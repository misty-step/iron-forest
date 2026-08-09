package main

import (
	"strings"
	"testing"
	"time"

	"github.com/misty-step/iron-forest/core"
)

func TestWatchIncludesManagerRuns(t *testing.T) {
	groups := groupRuns([]core.RunRecord{{
		Time: "2026-08-08T12:34:56Z", Flow: "manager", Subject: "item-23",
		Revision: strings.Repeat("a", 40), Status: "reaped", Agent: "manager",
	}}, 8)
	if len(groups["manager"]) != 1 {
		t.Fatalf("Manager runs = %#v, want one", groups["manager"])
	}

	var frame strings.Builder
	renderWatch(&frame, watchSnapshot{
		DrawnAt: time.Date(2026, 8, 8, 12, 34, 56, 0, time.UTC),
		Repo:    "misty-step/iron-forest",
		Flows:   groups,
	})
	output := frame.String()
	if !strings.Contains(output, "MANAGER (1 recent runs)") || !strings.Contains(output, "item-23") {
		t.Fatalf("Manager watch frame omitted run:\n%s", output)
	}
}
