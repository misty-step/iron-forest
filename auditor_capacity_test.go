package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestNoteDecoderOmitsHugeKeyFromError(t *testing.T) {
	sha := strings.Repeat("e", 40)
	hugeKey := strings.Repeat("z", 8*1024)
	payload := `{"schema":"forest.review-request.v1","` + hugeKey + `":1,"issue":1,"branch":"forest/1-gate","revision":"` + sha + `","time":"2026-08-10T00:00:00Z"}`
	if _, err := decodeReview([]byte(payload), sha); err == nil {
		t.Fatal("huge unknown key accepted")
	} else if len(err.Error()) > auditorViolationEntryBytes {
		t.Fatalf("strict-decoder error unbounded: %d bytes", len(err.Error()))
	} else if !strings.Contains(err.Error(), "unknown JSON object key") {
		t.Fatalf("key classification lost: %v", err)
	}
}

func TestViolationCollectorBoundsAt999WithOmissionSummary(t *testing.T) {
	var collector violationCollector
	const total = 1002
	for range total {
		collector.add("violation")
	}
	got := collector.finalize()
	wantLen := auditorConcreteViolations + 1
	if len(got) != wantLen {
		t.Fatalf("finalized entries=%d want %d", len(got), wantLen)
	}
	summary := fmt.Sprintf("%d additional violations omitted", total-auditorConcreteViolations)
	if got[len(got)-1] != summary {
		t.Fatalf("summary=%q want %q", got[len(got)-1], summary)
	}
}

func TestViolationCollectorTruncatesOversizedEntry(t *testing.T) {
	var collector violationCollector
	collector.add(strings.Repeat("x", auditorViolationEntryBytes*2))
	got := collector.finalize()
	if len(got) != 1 || len(got[0]) > auditorViolationEntryBytes {
		t.Fatalf("truncated entries=%q", got)
	}
}
