package main

import (
	"os"
	"strings"
	"testing"
)

// The second-party deployment checklist is an executable handoff contract, so
// the docs regressions that previously let it drift from the real service
// lifecycle are pinned here as content checks. They are scoped to the checklist
// subsection rather than the whole README, because later sections legitimately
// document `./forest serve` for a manual foreground deployment and `audit show
// --rescan` as a lock-taking read-surface command.
func readmeChecklistSection(t *testing.T) string {
	t.Helper()

	body, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	content := string(body)

	start := strings.Index(content, "### Second-party deployment checklist")
	if start < 0 {
		t.Fatal("second-party deployment checklist heading not found in README.md")
	}
	content = content[start:]

	end := strings.Index(content, "Build with the pinned toolchain")
	if end < 0 {
		t.Fatal("second-party deployment checklist end marker not found in README.md")
	}
	return content[:end]
}

func TestSecondPartyChecklistDoesNotStartSecondKernel(t *testing.T) {
	checklist := readmeChecklistSection(t)

	if strings.Contains(checklist, "`./forest serve`") {
		t.Fatal("checklist must not instruct starting a second Kernel with `./forest serve`")
	}
	if strings.Contains(checklist, "Start exactly one Kernel") {
		t.Fatal("checklist must not use the obsolete 'start exactly one Kernel' wording")
	}
	if !strings.Contains(checklist, "`systemctl --user is-active forest@<sibling-directory-name>`") {
		t.Fatal("checklist must verify service liveness with `systemctl --user is-active`")
	}
	if !strings.Contains(checklist, "`./forest status`") {
		t.Fatal("checklist must verify service liveness with `./forest status`")
	}
}

func TestSecondPartyChecklistRescanStopsServiceFirst(t *testing.T) {
	checklist := readmeChecklistSection(t)

	rescan := strings.Index(checklist, "`./forest audit show --rescan`")
	if rescan < 0 {
		t.Fatal("checklist must document the rescan procedure")
	}
	stop := strings.Index(checklist, "`systemctl --user stop forest@<sibling-directory-name>`")
	start := strings.Index(checklist, "`systemctl --user start forest@<sibling-directory-name>`")
	if stop < 0 || start < 0 {
		t.Fatal("checklist rescan sequence must stop and start the service")
	}
	if !(stop < rescan && rescan < start) {
		t.Fatal("checklist must stop the service before `--rescan` and start it after")
	}
}
