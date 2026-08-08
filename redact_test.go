package main

import (
	"strings"
	"testing"
)

// TestRedactSecretShapedVendorKeypins item #120's core oracle: a sk- shaped
// token planted in a report must vanish entirely, leaving no sk- substring that
// a pull request, a comment, or a remote note could carry.
func TestRedactSecretShapedVendorKey(t *testing.T) {
	got := redactSecretShaped("built with key sk-AAAAAAAAAAAAAAAA against master")
	if strings.Contains(got, "sk-") {
		t.Fatalf("redacted text still contains sk-: %q", got)
	}
	if !strings.Contains(got, secretRedacted) {
		t.Fatalf("redacted text lacks the marker: %q", got)
	}
}

func TestRedactSecretShapedMintMarkerAndProxyHost(t *testing.T) {
	marker := `"apiKey": "__mint.openrouter.ironforest__"`
	if got := redactSecretShaped(marker); strings.Contains(got, "__mint.") || !strings.Contains(got, secretRedacted) {
		t.Fatalf("mint marker not redacted: %q", got)
	}
	url := "http://mint.tail5f5eb4.ts.net:4949/proxy/https/openrouter.ai/api/v1"
	got := redactSecretShaped(url)
	if strings.Contains(got, "mint.tail5f5eb4") || !strings.Contains(got, secretRedacted) {
		t.Fatalf("proxy host not redacted: %q", got)
	}
}

func TestRedactSecretShapedOtherPrefixes(t *testing.T) {
	for _, input := range []string{
		"token ghp_abcdefghijklmnopqrstuvwxyz1234567890",
		"AKIAABCDEFGHIJKLMNOP",
		"sk_live_1234567890abcdef",
	} {
		if got := redactSecretShaped(input); got == input || !strings.Contains(got, secretRedacted) {
			t.Fatalf("expected full redaction for %q, got %q", input, got)
		}
	}
}

// TestRedactSecretShapedLeavesBenignText pins the inverse: ordinary prose must
// round-trip byte-for-byte so a clean run publishes exactly what the agent wrote.
func TestRedactSecretShapedLeavesBenignText(t *testing.T) {
	benign := "Generated for item #7: add notes.\n\nFixed the parser.\n\nChanged files:\n- parser.go\n"
	if got := redactSecretShaped(benign); got != benign {
		t.Fatalf("benign text was altered:\n%q\nvs\n%q", got, benign)
	}
	if secretShaped(benign) {
		t.Fatalf("benign text reported as secret-shaped")
	}
	if !secretShaped("sk-AAAAAAAAAAAAAAAA") {
		t.Fatalf("secret-shaped text not detected")
	}
}

// TestRedactSecretShapedBuildsNotesVerdictsEndToEnd proves the redaction reaches
// the exact outbound sinks the item names: the pull-request body, the verdict
// note object, and the comment projection body.
func TestRedactSecretShapedOutboundSinks(t *testing.T) {
	rep := report{
		Summary: "sk-AAAAAAAAAAAAAAAA in here",
		Notes:   "proxy http://mint.tail5f5eb4.ts.net:4949/proxy/x",
	}
	body := builderProjectionBody(Item{ID: "7", Title: "title"}, rep, []string{"a.go"})
	if strings.Contains(body, "sk-") || strings.Contains(body, "mint.tail5f5eb4") {
		t.Fatalf("pull-request body leaked a secret: %q", body)
	}

	vb := verdictBody(
		verdictNote{Verdict: "changes", Notes: "see sk-AAAAAAAAAAAAAAAA"},
		checksNote{Status: "fail", Results: []checkResult{{Name: "test", Code: 1, Output: "got sk-AAAAAAAAAAAAAAAA"}}},
	)
	if strings.Contains(vb, "sk-") {
		t.Fatalf("comment projection leaked a secret: %q", vb)
	}
}

// TestRedactSecretShapedNotePersistence proves a verdict note whose notes carry a
// secret is stored without the secret on the remote, not just redacted at
// display time: the scribed fact itself carries no sk- substring.
func TestRedactSecretShapedNotePersistence(t *testing.T) {
	_, work, sha := notesTestRepository(t)
	if err := writeVerdict(work, sha, verdictNote{
		Verdict: "approve", Notes: "key is sk-AAAAAAAAAAAAAAAA", Reviewer: "r", RunID: "run-x",
	}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := readVerdict(work, sha)
	if err != nil || !ok {
		t.Fatalf("readVerdict = (found=%v, err=%v)", ok, err)
	}
	if strings.Contains(got.Notes, "sk-") {
		t.Fatalf("durable verdict note carries a secret: %q", got.Notes)
	}
	raw := runGitTest(t, work, "notes", "--ref=forest/verdict", "show", sha)
	if strings.Contains(raw, "sk-") {
		t.Fatalf("raw remote note carries a secret: %q", raw)
	}
}
