package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	evidenceRefPrefix        = "refs/forest/v1/"
	evidenceRequestRefPrefix = "refs/forest/v1/request/"
	evidenceChecksRefPrefix  = "refs/forest/v1/checks/"
	evidenceVerdictRefPrefix = "refs/forest/v1/verdict/"
)

type publishVerdictInput struct {
	Root        string
	ChecksPath  string
	VerdictPath string
	RunID       string
}

type publishVerdictResult struct {
	Status   string `json:"status"`
	Revision string `json:"revision"`
	Verdict  string `json:"verdict"`
}

func runPublishVerdict(rest []string, flags cliFlags) cliOutcome {
	result, err := publishVerdict(context.Background(), publishVerdictInput{
		Root:        flags.root,
		ChecksPath:  rest[0],
		VerdictPath: rest[1],
		RunID:       strings.TrimSpace(os.Getenv("FOREST_RUN_ID")),
	})
	if err != nil {
		if publishConflict(err) {
			return failure(exitConflict, "%s", err)
		}
		return failure(exitError, "%s", err)
	}
	human := fmt.Sprintf("published %s verdict for %s", result.Verdict, result.Revision)
	if result.Status == "identical" {
		human = fmt.Sprintf("accepted identical %s verdict for %s", result.Verdict, result.Revision)
	}
	return cliOutcome{Exit: exitOK, Data: result, Human: human}
}

func publishVerdict(ctx context.Context, input publishVerdictInput) (publishVerdictResult, error) {
	input.RunID = strings.TrimSpace(input.RunID)
	if !validPublicationRunID(input.RunID) {
		return publishVerdictResult{}, fmt.Errorf("FOREST_RUN_ID must identify a valid Verifier run")
	}
	runRoot, err := primaryCheckout(ctx, input.Root)
	if err != nil {
		return publishVerdictResult{}, fmt.Errorf("resolve publication checkout: %w", err)
	}
	if err := requireNativeDelivery(runRoot); err != nil {
		return publishVerdictResult{}, err
	}
	run, err := requirePublicationRun(runRoot, "verifier", input.RunID)
	if err != nil {
		return publishVerdictResult{}, err
	}
	checksPath, err := filepath.Abs(input.ChecksPath)
	if err != nil {
		return publishVerdictResult{}, err
	}
	verdictPath, err := filepath.Abs(input.VerdictPath)
	if err != nil {
		return publishVerdictResult{}, err
	}
	checksData, err := os.ReadFile(checksPath)
	if err != nil {
		return publishVerdictResult{}, fmt.Errorf("read checks payload: %w", err)
	}
	verdictData, err := os.ReadFile(verdictPath)
	if err != nil {
		return publishVerdictResult{}, fmt.Errorf("read verdict payload: %w", err)
	}
	revision, err := payloadRevision(verdictData)
	if err != nil {
		return publishVerdictResult{}, fmt.Errorf("invalid verdict note")
	}
	verdict, err := decodeVerdict(verdictData, revision)
	if err != nil {
		return publishVerdictResult{}, err
	}
	checks, err := decodeChecks(checksData, revision)
	if err != nil {
		return publishVerdictResult{}, err
	}
	if verdict.Verdict == "approve" {
		if err := requirePassingApprovalChecks(checks); err != nil {
			return publishVerdictResult{}, err
		}
	}
	cfg, err := loadConfig(configPath(runRoot))
	if err != nil {
		return publishVerdictResult{}, err
	}
	poller := input.Powder
	if poller == nil {
		poller = NewPoller(input.Root, cfg.Repo, Scope{})
	}
	request, requestOID, err := requireVerdictRequest(ctx, poller, revision, run)
	if err != nil {
		return publishVerdictResult{}, err
	}

	checksRef := evidenceChecksRefPrefix + revision
	verdictRef := evidenceVerdictRefPrefix + revision
	existingChecks, err := remoteOID(ctx, input.Root, checksRef)
	if err != nil {
		return publishVerdictResult{}, err
	}
	existingVerdict, err := remoteOID(ctx, input.Root, verdictRef)
	if err != nil {
		return publishVerdictResult{}, err
	}
	identical, err := identicalEvidence(ctx, input.Root, checksRef, verdictRef, existingChecks, existingVerdict, checksData, verdictData, input.RunID)
	if err != nil {
		return publishVerdictResult{}, err
	}
	if identical {
		if err := requireUnchangedPublicationRun(runRoot, run); err != nil {
			return publishVerdictResult{}, err
		}
		if err := requireNativeDelivery(runRoot); err != nil {
			return publishVerdictResult{}, err
		}
		return publishVerdictResult{Status: "identical", Revision: revision, Verdict: verdict.Verdict}, nil
	}
	if existingChecks != "" || existingVerdict != "" {
		return publishVerdictResult{}, conflictError("conflicting evidence ref for %s", revision)
	}

	if err := requireReviewRequestTip(ctx, input.Root, request); err != nil {
		return publishVerdictResult{}, err
	}
	var primaryRef string
	if verdict.Verdict == "approve" {
		primaryRef, _, err = resolvePrimary(ctx, input.Root, cfg)
		if err != nil {
			return publishVerdictResult{}, fmt.Errorf("resolve primary ref: %w", err)
		}
		if err := runConfiguredChecksWithAttestation(ctx, input.Root, revision, checks.Results); err != nil {
			return publishVerdictResult{}, err
		}
	}

	checksCommit, err := commitEvidence(ctx, input.Root, "checks.json", checksData, "forest checks "+revision)
	if err != nil {
		return publishVerdictResult{}, err
	}
	verdictCommit, err := commitEvidence(ctx, input.Root, "verdict.json", verdictData, "forest verdict "+revision)
	if err != nil {
		return publishVerdictResult{}, err
	}
	args := []string{
		"push", "--atomic",
		"--force-with-lease=" + checksRef + ":",
		"--force-with-lease=" + verdictRef + ":",
	}
	requestRef := evidenceRequestRefPrefix + revision
	branchRef := "refs/heads/" + request.Branch
	args = append(args,
		"--force-with-lease="+requestRef+":"+requestOID,
		"--force-with-lease="+branchRef+":"+revision,
	)
	args = append(args,
		"origin",
		checksCommit+":"+checksRef,
		verdictCommit+":"+verdictRef,
		requestOID+":"+requestRef,
		revision+":"+branchRef,
	)
	if verdict.Verdict == "approve" {
		args = append(args, revision+":"+primaryRef)
	}
	if err := poller.confirmEvidence(ctx, branchTip{Name: request.Branch, SHA: revision}, requestOID, ""); err != nil {
		return publishVerdictResult{}, conflictError("candidate changed before publication: %w", err)
	}
	if err := requireNativeDelivery(runRoot); err != nil {
		return publishVerdictResult{}, err
	}
	// Checks can outlive their owning Run. Never publish after that owner ends
	// or is replaced, even if every candidate check passed.
	if err := requireUnchangedPublicationRun(runRoot, run); err != nil {
		return publishVerdictResult{}, err
	}
	if err := gitRun(ctx, input.Root, args...); err != nil {
		return publishVerdictResult{}, classifyVerdictPush(err)
	}
	result := publishVerdictResult{Status: "published", Revision: revision, Verdict: verdict.Verdict}
	return result, nil
}

func requirePassingApprovalChecks(checks checksNote) error {
	for _, result := range checks.Results {
		if !result.OK || result.Exit != 0 {
			return fmt.Errorf("cannot approve with failing Checks result %q (ok=%t exit=%d)", result.Name, result.OK, result.Exit)
		}
	}
	return nil
}

func requireVerdictRequest(ctx context.Context, poller *Poller, revision string, run liveRunRecord) (reviewRequest, string, error) {
	data, oid, err := poller.evidencePayloadAndOID(ctx, "request", revision, "builder", "fixer")
	if err != nil {
		return reviewRequest{}, "", fmt.Errorf("read request evidence for verdict: %w", err)
	}
	request, err := decodeReview(data, revision)
	if err != nil {
		return request, "", fmt.Errorf("invalid request evidence for verdict: %w", err)
	}
	if !sameWorkReference(request.Work, run.Work) || request.RunID == run.RunID {
		return request, "", fmt.Errorf("Verifier Run must independently review the same work as the request")
	}
	return request, oid, nil
}

func requireReviewRequestTip(ctx context.Context, root string, request reviewRequest) error {
	tip, err := remoteOID(ctx, root, "refs/heads/"+request.Branch)
	if err != nil {
		return fmt.Errorf("read request branch tip: %w", err)
	}
	if tip != request.Revision {
		return conflictError("request branch %q tip %q does not match revision %s", request.Branch, tip, request.Revision)
	}
	return nil
}
func payloadRevision(data []byte) (string, error) {
	var payload struct {
		Revision string `json:"revision"`
	}
	if err := decodeStrictJSON(data, &payload, objectJSONShape("schema", "revision", "verdict", "summary", "time")); err != nil {
		return "", err
	}
	if !isSHA(payload.Revision) {
		return "", fmt.Errorf("missing revision")
	}
	return payload.Revision, nil
}

func commitEvidence(ctx context.Context, root, name string, payload []byte, message string) (string, error) {
	return commitEvidenceAs(ctx, root, name, payload, message, "Iron Forest Verifier", "verifier@forest.invalid")
}

func commitEvidenceAs(ctx context.Context, root, name string, payload []byte, message, author, email string) (string, error) {
	blob, err := gitHashObject(ctx, root, payload)
	if err != nil {
		return "", err
	}
	tree, err := gitLineInput(ctx, root, []byte("100644 blob "+blob+"\t"+name+"\n"), "mktree")
	if err != nil {
		return "", err
	}
	return gitCommitTreeAs(ctx, root, tree, message, author, email)
}

func gitHashObject(ctx context.Context, root string, payload []byte) (string, error) {
	output, err := gitOutputInput(ctx, root, payload, "hash-object", "-w", "--stdin")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func gitLineInput(ctx context.Context, root string, input []byte, args ...string) (string, error) {
	output, err := gitOutputInput(ctx, root, input, args...)
	if err != nil {
		return "", err
	}
	line, err := exactGitLine(output)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return line, nil
}

func gitCommitTreeAs(ctx context.Context, root, tree, message, author, email string) (string, error) {
	path, err := trustedExecutable(root, "git")
	if err != nil {
		return "", err
	}
	command := exec.CommandContext(ctx, path, "-C", root, "commit-tree", tree, "-m", message)
	command.Env = publicationGitEnv(author, email)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git commit-tree: %w\n%s", err, stderr.Bytes())
	}
	line, err := exactGitLine(stdout.Bytes())
	if err != nil {
		return "", fmt.Errorf("git commit-tree: %w", err)
	}
	return line, nil
}

func identicalEvidence(ctx context.Context, root, checksRef, verdictRef, checksOID, verdictOID string, checksData, verdictData []byte, runID string) (bool, error) {
	if checksOID == "" && verdictOID == "" {
		return false, nil
	}
	if checksOID == "" || verdictOID == "" {
		return false, nil
	}
	prefix := "refs/forest/private/" + runID + "/compare/"
	gotChecks, err := evidenceBlob(ctx, root, checksRef, "checks.json", prefix)
	if err != nil {
		return false, err
	}
	gotVerdict, err := evidenceBlob(ctx, root, verdictRef, "verdict.json", prefix)
	if err != nil {
		return false, err
	}
	return bytes.Equal(gotChecks, checksData) && bytes.Equal(gotVerdict, verdictData), nil
}

func evidenceBlob(ctx context.Context, root, ref, name, privatePrefix string) ([]byte, error) {
	local, err := newPrivateEvidenceRef(privatePrefix)
	if err != nil {
		return nil, err
	}
	if err := gitRun(ctx, root, "fetch", "origin", ref+":"+local); err != nil {
		return nil, err
	}
	defer func() { _ = gitRun(ctx, root, "update-ref", "-d", local) }()
	return gitOutput(ctx, root, "show", local+":"+name)
}

func classifyVerdictPush(err error) error {
	text := err.Error()
	if strings.Contains(text, "non-fast-forward") || strings.Contains(text, "stale info") || strings.Contains(text, "failed to push") || strings.Contains(text, "rejected") {
		return conflictError("%s", text)
	}
	return err
}
