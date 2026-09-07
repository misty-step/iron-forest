package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type powderCommandFunc func(context.Context, ...string) ([]byte, []byte, error)

type powderReconcileResult struct {
	Revision string
	Subject  string
	Tracker  string
	Powder   bool
	Terminal bool
}

type powderJob struct {
	ID    string `json:"id"`
	Repo  string `json:"repo"`
	Proof string `json:"proof"`
	Lease *struct {
		Agent string `json:"agent"`
	} `json:"lease"`
	Derived struct {
		Terminal *bool `json:"terminal"`
	} `json:"derived"`
}

type powderError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func (p *Poller) runPowderCommand(ctx context.Context, args ...string) ([]byte, []byte, error) {
	if p.PowderCommand != nil {
		return p.PowderCommand(ctx, args...)
	}
	if !p.ResolveTools {
		output, err := p.powder(ctx, args...)
		return output, nil, err
	}
	path, err := trustedExecutable(p.Root, "powder")
	if err != nil {
		return nil, nil, err
	}
	command := exec.Command(path, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	stdout, err := processGroupOutput(ctx, command)
	return stdout, stderr.Bytes(), err
}

func decodePowderError(data []byte) (powderError, bool) {
	var payload powderError
	if json.Unmarshal(data, &payload) != nil || strings.TrimSpace(payload.Code) == "" {
		return powderError{}, false
	}
	return payload, true
}

func powderCommandFailure(command, subject string, stderr []byte, err error) error {
	if payload, ok := decodePowderError(stderr); ok {
		return fmt.Errorf("powder %s %s: %s (%s): %w", command, subject, payload.Error, payload.Code, err)
	}
	return fmt.Errorf("powder %s %s: %w", command, subject, err)
}

func (p *Poller) showPowderJob(ctx context.Context, subject string) (powderJob, bool, error) {
	stdout, stderr, err := p.runPowderCommand(ctx, "show", subject)
	if err != nil {
		if payload, ok := decodePowderError(stderr); ok && payload.Code == "not_found" {
			return powderJob{}, false, nil
		}
		return powderJob{}, false, powderCommandFailure("show", subject, stderr, err)
	}
	var job powderJob
	if err := json.Unmarshal(stdout, &job); err != nil {
		return powderJob{}, false, fmt.Errorf("malformed powder show for %s: %w", subject, err)
	}
	if strings.TrimSpace(job.ID) == "" || strings.TrimSpace(job.Repo) == "" || job.Derived.Terminal == nil {
		return powderJob{}, false, fmt.Errorf("malformed powder show for %s", subject)
	}
	return job, true, nil
}

func (p *Poller) currentPowderLanding(ctx context.Context) (powderReconcileResult, error) {
	cfg, err := loadConfig(configPath(p.Root))
	if err != nil {
		return powderReconcileResult{}, err
	}
	primaryRef, _, err := resolvePrimary(ctx, p.Root, cfg)
	if err != nil {
		return powderReconcileResult{}, fmt.Errorf("resolve primary ref: %w", err)
	}
	primaryOutput, err := p.git(ctx, "ls-remote", "origin", primaryRef)
	if err != nil {
		return powderReconcileResult{}, err
	}
	revision, err := parseSingleRemoteOID(primaryOutput, primaryRef)
	if err != nil {
		return powderReconcileResult{}, err
	}
	if revision == "" {
		return powderReconcileResult{}, fmt.Errorf("primary ref %s is missing", primaryRef)
	}
	result := powderReconcileResult{Revision: revision}
	requestData, requestErr := p.evidencePayload(ctx, "request", revision, "builder", "fixer")
	verdictData, verdictErr := p.evidencePayload(ctx, "verdict", revision, "verifier")
	requestMissing := isMissingNote(requestErr)
	verdictMissing := isMissingNote(verdictErr)
	if requestMissing && verdictMissing {
		return result, nil
	}
	if requestErr != nil && !requestMissing {
		return result, requestErr
	}
	if verdictErr != nil && !verdictMissing {
		return result, verdictErr
	}
	if requestMissing || verdictMissing {
		return result, fmt.Errorf("current primary %s has incomplete Gate evidence", revision)
	}

	var schema struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(requestData, &schema); err != nil {
		return result, fmt.Errorf("invalid current primary request: %w", err)
	}
	if schema.Schema == "forest.review-request.v1" {
		if _, err := decodeLegacyReview(requestData, revision); err != nil {
			return result, err
		}
		return result, nil
	}
	request, err := decodeReview(requestData, revision)
	if err != nil {
		return result, err
	}
	verdict, err := decodeVerdict(verdictData, revision)
	if err != nil {
		return result, err
	}
	if verdict.Verdict != "approve" {
		return result, fmt.Errorf("current primary %s has non-approve verdict", revision)
	}
	result.Subject = request.Subject
	result.Tracker = request.Tracker
	return result, nil
}

func (p *Poller) reconcilePowderPrimary(ctx context.Context) (powderReconcileResult, error) {
	agent := powderAgent()
	if !p.ResolveTools && p.PowderCommand == nil {
		// An injected tool transport must also inject the state-changing Powder
		// transport. Production Pollers always resolve both trusted binaries.
		return powderReconcileResult{}, nil
	}
	if !powderOriginSet() {
		if agent != "" {
			return powderReconcileResult{}, fmt.Errorf("POWDER_AGENT is set but POWDER_URL and POWDER_API_BASE_URL are empty")
		}
		return powderReconcileResult{}, nil
	}
	result, err := p.currentPowderLanding(ctx)
	if err != nil || result.Subject == "" || result.Tracker != "powder" {
		return result, err
	}
	job, exists, err := p.showPowderJob(ctx, result.Subject)
	if err != nil {
		return result, err
	}
	if !exists {
		return result, fmt.Errorf("powder show %s: job not found", result.Subject)
	}
	result.Powder = true
	if job.ID != result.Subject {
		return result, fmt.Errorf("powder show returned job %q for Subject %q", job.ID, result.Subject)
	}
	if job.Repo != p.Repo {
		return result, fmt.Errorf("powder job %s belongs to %q, want %q", result.Subject, job.Repo, p.Repo)
	}
	if *job.Derived.Terminal {
		return acceptTerminalPowder(job, result)
	}

	takeArgs := []string{"take", result.Subject}
	if agent != "" {
		takeArgs = append(takeArgs, "--agent", agent)
	}
	if _, stderr, err := p.runPowderCommand(ctx, takeArgs...); err != nil {
		return result, powderCommandFailure("take", result.Subject, stderr, err)
	}
	doneArgs := []string{"done", result.Subject, "--proof", result.Revision}
	if agent != "" {
		doneArgs = append(doneArgs, "--agent", agent)
	}
	if _, stderr, err := p.runPowderCommand(ctx, doneArgs...); err != nil {
		return result, powderCommandFailure("done", result.Subject, stderr, err)
	}
	job, exists, err = p.showPowderJob(ctx, result.Subject)
	if err != nil {
		return result, err
	}
	if !exists || job.ID != result.Subject || job.Repo != p.Repo || !*job.Derived.Terminal {
		return result, fmt.Errorf("powder job %s did not become terminal", result.Subject)
	}
	return acceptTerminalPowder(job, result)
}

func acceptTerminalPowder(job powderJob, result powderReconcileResult) (powderReconcileResult, error) {
	if job.Proof != result.Revision {
		return result, fmt.Errorf("powder job %s proof %q does not match revision %q", result.Subject, job.Proof, result.Revision)
	}
	result.Terminal = true
	return result, nil
}
