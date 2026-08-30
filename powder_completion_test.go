package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakePowderLifecycle struct {
	subject               string
	repo                  string
	leaseAgent            string
	terminal              bool
	proof                 string
	doneFailure           int
	doneResponseLoss      int
	corruptProofAfterDone bool
	calls                 []string
}

func (f *fakePowderLifecycle) command(_ context.Context, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) < 2 {
		return nil, nil, errors.New("missing Powder command")
	}
	command, subject := args[0], args[1]
	if command == "show" && subject != f.subject {
		return nil, []byte(`{"code":"not_found","error":"job not found"}`), errors.New("exit status 1")
	}
	if subject != f.subject {
		return nil, []byte(`{"code":"not_found","error":"job not found"}`), errors.New("exit status 1")
	}
	switch command {
	case "show":
		lease := any(nil)
		if f.leaseAgent != "" {
			lease = map[string]string{"agent": f.leaseAgent}
		}
		payload, err := json.Marshal(map[string]any{
			"id":      f.subject,
			"repo":    f.repo,
			"proof":   f.proof,
			"lease":   lease,
			"derived": map[string]bool{"terminal": f.terminal},
		})
		return payload, nil, err
	case "take":
		agent := argumentValue(args, "--agent")
		if f.leaseAgent != "" && f.leaseAgent != agent {
			return nil, []byte(`{"code":"already_holding","error":"held by another agent"}`), errors.New("exit status 1")
		}
		f.leaseAgent = agent
		return []byte(`{"id":"` + subject + `"}`), nil, nil
	case "done":
		if f.doneResponseLoss > 0 {
			f.doneResponseLoss--
			f.proof = argumentValue(args, "--proof")
			f.terminal = true
			f.leaseAgent = ""
			return nil, []byte(`{"code":"unavailable","error":"response lost"}`), errors.New("exit status 1")
		}
		if f.doneFailure > 0 {
			f.doneFailure--
			return nil, []byte(`{"code":"unavailable","error":"temporary outage"}`), errors.New("exit status 1")
		}
		f.proof = argumentValue(args, "--proof")
		if f.corruptProofAfterDone {
			f.proof = strings.Repeat("c", 40)
		}
		f.terminal = true
		f.leaseAgent = ""
		return []byte(`{"id":"` + subject + `"}`), nil, nil
	default:
		return nil, nil, fmt.Errorf("unexpected Powder command %q", command)
	}
}

func argumentValue(args []string, name string) string {
	for index, arg := range args {
		if arg == name && index+1 < len(args) {
			return args[index+1]
		}
	}
	return ""
}

func reviewRequestJSON(subject, revision, tracker string) string {
	payload := `{"schema":"forest.review-request.v2","subject":"` + subject + `","branch":"forest/` + subject + `/work","revision":"` + revision + `","time":"2026-08-29T00:00:00Z"`
	if tracker != "" {
		payload += `,"tracker":"` + tracker + `"`
	}
	return payload + `}`
}

func seedApprovedCurrent(t *testing.T, root, subject string) string {
	return seedApprovedCurrentTracker(t, root, subject, "powder")
}

func seedApprovedCurrentTracker(t *testing.T, root, subject, tracker string) string {
	t.Helper()
	revision := strings.TrimSpace(string(runGitDir(t, root, "rev-parse", "HEAD")))
	request := reviewRequestJSON(subject, revision, tracker)
	verdict := `{"schema":"forest.verdict.v1","revision":"` + revision + `","verdict":"approve","summary":"approved","time":"2026-08-29T00:00:01Z"}`
	pushEvidence(t, root, "request", revision, request, "Iron Forest Builder", "builder@forest.invalid")
	pushEvidence(t, root, "verdict", revision, verdict, "Iron Forest Verifier", "verifier@forest.invalid")
	return revision
}

func configuredPowderPoller(t *testing.T, root, subject string, lifecycle *fakePowderLifecycle) *Poller {
	t.Helper()
	t.Setenv("POWDER_AGENT", "forest-owner-name")
	t.Setenv("POWDER_URL", "https://powder.example")
	lifecycle.subject = subject
	lifecycle.repo = "owner/name"
	poller := NewPoller(root, "owner/name", Scope{})
	poller.PowderCommand = lifecycle.command
	return poller
}

func TestReconcilePowderPrimaryCompletesCurrentSubject(t *testing.T) {
	root, _ := testClone(t)
	revision := seedApprovedCurrent(t, root, "if-ready")
	lifecycle := &fakePowderLifecycle{leaseAgent: "forest-owner-name"}
	poller := configuredPowderPoller(t, root, "if-ready", lifecycle)

	result, err := poller.reconcilePowderPrimary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Powder || !result.Terminal || result.Subject != "if-ready" || result.Revision != revision {
		t.Fatalf("result=%#v", result)
	}
	if lifecycle.proof != revision {
		t.Fatalf("proof=%q want %q", lifecycle.proof, revision)
	}
	want := []string{
		"show if-ready",
		"take if-ready --agent forest-owner-name",
		"done if-ready --proof " + revision + " --agent forest-owner-name",
		"show if-ready",
	}
	if strings.Join(lifecycle.calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls=%v want %v", lifecycle.calls, want)
	}
}

func TestReconcilePowderPrimaryCompletesExplicitDecimalPowderSubject(t *testing.T) {
	root, _ := testClone(t)
	revision := seedApprovedCurrentTracker(t, root, "100", "powder")
	lifecycle := &fakePowderLifecycle{leaseAgent: "forest-owner-name"}
	poller := configuredPowderPoller(t, root, "100", lifecycle)
	result, err := poller.reconcilePowderPrimary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Powder || !result.Terminal || result.Subject != "100" || lifecycle.proof != revision {
		t.Fatalf("result=%#v proof=%q", result, lifecycle.proof)
	}
}

func TestReconcilePowderPrimaryRetriesAfterDoneFailure(t *testing.T) {
	root, _ := testClone(t)
	revision := seedApprovedCurrent(t, root, "if-retry")
	lifecycle := &fakePowderLifecycle{doneFailure: 1}
	poller := configuredPowderPoller(t, root, "if-retry", lifecycle)

	if _, err := poller.reconcilePowderPrimary(context.Background()); err == nil || !strings.Contains(err.Error(), "temporary outage") {
		t.Fatalf("first reconciliation error=%v", err)
	}
	result, err := poller.reconcilePowderPrimary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Terminal || lifecycle.proof != revision {
		t.Fatalf("result=%#v proof=%q", result, lifecycle.proof)
	}
}

func TestReconcilePowderPrimaryObservesTerminalAfterLostDoneResponse(t *testing.T) {
	root, _ := testClone(t)
	revision := seedApprovedCurrent(t, root, "if-response-loss")
	lifecycle := &fakePowderLifecycle{doneResponseLoss: 1}
	poller := configuredPowderPoller(t, root, "if-response-loss", lifecycle)

	if _, err := poller.reconcilePowderPrimary(context.Background()); err == nil || !strings.Contains(err.Error(), "response lost") {
		t.Fatalf("first reconciliation error=%v", err)
	}
	result, err := poller.reconcilePowderPrimary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !result.Terminal || lifecycle.proof != revision {
		t.Fatalf("result=%#v proof=%q", result, lifecycle.proof)
	}
	if got := lifecycle.calls[len(lifecycle.calls)-1]; got != "show if-response-loss" {
		t.Fatalf("last call=%q calls=%v", got, lifecycle.calls)
	}
}

func TestReconcilePowderPrimarySkipsGitHubAndTerminalSubjects(t *testing.T) {
	t.Run("github tracker never probes colliding powder id", func(t *testing.T) {
		root, _ := testClone(t)
		seedApprovedCurrentTracker(t, root, "100", "github")
		lifecycle := &fakePowderLifecycle{}
		poller := configuredPowderPoller(t, root, "100", lifecycle)
		result, err := poller.reconcilePowderPrimary(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Powder || result.Subject != "100" || result.Tracker != "github" || len(lifecycle.calls) != 0 {
			t.Fatalf("result=%#v calls=%v", result, lifecycle.calls)
		}
	})

	t.Run("undiscriminated historical request never probes powder", func(t *testing.T) {
		root, _ := testClone(t)
		seedApprovedCurrentTracker(t, root, "if-ready", "")
		lifecycle := &fakePowderLifecycle{}
		poller := configuredPowderPoller(t, root, "if-ready", lifecycle)
		result, err := poller.reconcilePowderPrimary(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if result.Powder || result.Tracker != "" || len(lifecycle.calls) != 0 {
			t.Fatalf("result=%#v calls=%v", result, lifecycle.calls)
		}
	})

	t.Run("already terminal with landed proof", func(t *testing.T) {
		root, _ := testClone(t)
		revision := seedApprovedCurrent(t, root, "if-done")
		lifecycle := &fakePowderLifecycle{terminal: true, proof: revision}
		poller := configuredPowderPoller(t, root, "if-done", lifecycle)
		result, err := poller.reconcilePowderPrimary(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !result.Powder || !result.Terminal || len(lifecycle.calls) != 1 {
			t.Fatalf("result=%#v calls=%v", result, lifecycle.calls)
		}
	})
}

func TestReconcilePowderPrimaryRejectsMismatchedProof(t *testing.T) {
	t.Run("already terminal with other proof", func(t *testing.T) {
		root, _ := testClone(t)
		seedApprovedCurrent(t, root, "if-stale")
		lifecycle := &fakePowderLifecycle{terminal: true, proof: strings.Repeat("b", 40)}
		poller := configuredPowderPoller(t, root, "if-stale", lifecycle)
		if _, err := poller.reconcilePowderPrimary(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match revision") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("post-done show returns other proof", func(t *testing.T) {
		root, _ := testClone(t)
		seedApprovedCurrent(t, root, "if-corrupt")
		lifecycle := &fakePowderLifecycle{corruptProofAfterDone: true}
		poller := configuredPowderPoller(t, root, "if-corrupt", lifecycle)
		if _, err := poller.reconcilePowderPrimary(context.Background()); err == nil || !strings.Contains(err.Error(), "does not match revision") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestReconcilePowderPrimaryRejectsForeignLease(t *testing.T) {
	root, _ := testClone(t)
	seedApprovedCurrent(t, root, "if-held")
	lifecycle := &fakePowderLifecycle{leaseAgent: "forest-other"}
	poller := configuredPowderPoller(t, root, "if-held", lifecycle)
	if _, err := poller.reconcilePowderPrimary(context.Background()); err == nil || !strings.Contains(err.Error(), `held by "forest-other"`) {
		t.Fatalf("error=%v", err)
	}
	if len(lifecycle.calls) != 1 {
		t.Fatalf("calls=%v", lifecycle.calls)
	}
}

func TestBuilderPollFailsBeforeTrackerDispatchWhenPowderCompletionFails(t *testing.T) {
	root, _ := testClone(t)
	seedApprovedCurrent(t, root, "if-pending")
	lifecycle := &fakePowderLifecycle{doneFailure: 1}
	poller := configuredPowderPoller(t, root, "if-pending", lifecycle)
	code, err := poller.builder(context.Background())
	if code != exitError || err == nil || !strings.Contains(err.Error(), "temporary outage") {
		t.Fatalf("builder code=%d error=%v", code, err)
	}
}
