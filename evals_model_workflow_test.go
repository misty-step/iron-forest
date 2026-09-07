package main

import (
	"os"
	"strings"
	"testing"
)

// The scheduled `model evals` workflow is the live-model spending contract, so
// a regression here is caught by asserting the guards the rejected Revision
// got wrong: the monthly tier must be scheduled as a Monday-only cron and must
// be gated to the first Monday of the month. A cron with both day-of-month and
// day-of-week restricted fires when either matches, which would otherwise run
// the expensive monthly pass^3 tier up to ~11 times a month and then fail
// duplicate-fingerprint rejection.
func TestEvalsModelWorkflowMonthlyScheduleGuard(t *testing.T) {
	workflow, err := os.ReadFile(".github/workflows/evals-model.yml")
	if err != nil {
		t.Fatal(err)
	}
	content := string(workflow)

	if strings.Contains(content, `cron: "17 3 1-7 * 1"`) {
		t.Fatal("monthly schedule must not restrict both day-of-month and day-of-week; that cron fires on every day 1-7 and every Monday")
	}
	if !strings.Contains(content, `cron: "17 3 * * 1"`) {
		t.Fatal("monthly schedule must be a Monday-only cron, gated to the first Monday of the month")
	}
	if !strings.Contains(content, `github.event.schedule == '17 3 * * 1' && 'monthly'`) {
		t.Fatal("monthly tier must map to the Monday-only schedule, not a day-of-month/day-of-week OR schedule")
	}
	if !strings.Contains(content, `day_of_month="$(date +%-d)"`) {
		t.Fatal("monthly gate must inspect the current day of month")
	}
	if !strings.Contains(content, `day_of_week="$(date +%u)"`) {
		t.Fatal("monthly gate must inspect the current day of week")
	}
	if !strings.Contains(content, `if [ "$day_of_week" = "1" ] && [ "$day_of_month" -le 7 ]`) {
		t.Fatal("monthly gate must allow the experiment only on the first Monday (weekday 1 and day 1-7)")
	}
	if !strings.Contains(content, `steps.monthly-gate.outputs.monthly_allowed == 'true'`) {
		t.Fatal("monthly experiment step must be gated by the first-Monday resolution")
	}
	if !strings.Contains(content, `always() && steps.experiment.outcome != 'skipped'`) {
		t.Fatal("artifact retention must not require artifacts when the monthly experiment is skipped")
	}
}
