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
	Powder      *Poller
}

type publishVerdictResult struct {
	Status        string `json:"status"`
	Revision      string `json:"revision"`
	Verdict       string `json:"verdict"`
	PowderStatus  string `json:"powder_status,omitempty"`
	PowderSubject string `json:"powder_subject,omitempty"`
	PowderError   string `json:"powder_error,omitempty"`
}

func runPublishVerdict(rest []string, flags cliFlags) cliOutcome {
	result, err := publishVerdict(context.Background(), publishVerdictInput{
		Root:        flags.root,
		ChecksPath:  rest[0],
		VerdictPath: rest[1],
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
	if result.PowderStatus == "pending" {
		target := result.PowderSubject
		if target == "" {
			target = "current primary"
		}
		human += fmt.Sprintf("; Powder completion pending for %s: %s", target, result.PowderError)
	}
	return cliOutcome{Exit: exitOK, Data: result, Human: human}
}

func withPowderReconciliation(result publishVerdictResult, reconciliation powderReconcileResult, err error) publishVerdictResult {
	if err != nil {
		result.PowderStatus = "pending"
		result.PowderSubject = reconciliation.Subject
		result.PowderError = err.Error()
		return result
	}
	if reconciliation.Powder && reconciliation.Terminal {
		result.PowderStatus = "terminal"
		result.PowderSubject = reconciliation.Subject
	}
	return result
}

func publishVerdict(ctx context.Context, input publishVerdictInput) (publishVerdictResult, error) {
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
	if _, err := decodeChecks(checksData, revision); err != nil {
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
	identical, err := identicalEvidence(ctx, input.Root, checksRef, verdictRef, existingChecks, existingVerdict, checksData, verdictData)
	if err != nil {
		return publishVerdictResult{}, err
	}
	if identical {
		result := publishVerdictResult{Status: "identical", Revision: revision, Verdict: verdict.Verdict}
		if verdict.Verdict != "approve" {
			return result, nil
		}
		cfg, loadErr := loadConfig(configPath(input.Root))
		if loadErr != nil {
			return withPowderReconciliation(result, powderReconcileResult{}, loadErr), nil
		}
		powder := input.Powder
		if powder == nil {
			powder = NewPoller(input.Root, cfg.Repo, Scope{})
		}
		reconciliation, reconcileErr := powder.reconcilePowderPrimary(ctx)
		return withPowderReconciliation(result, reconciliation, reconcileErr), nil
	}
	if existingChecks != "" || existingVerdict != "" {
		return publishVerdictResult{}, conflictError("conflicting evidence ref for %s", revision)
	}

	var primaryRef string
	var powder *Poller
	if verdict.Verdict == "approve" {
		cfg, loadErr := loadConfig(configPath(input.Root))
		if loadErr != nil {
			return publishVerdictResult{}, loadErr
		}
		primaryRef, _, err = resolvePrimary(ctx, input.Root, cfg)
		if err != nil {
			return publishVerdictResult{}, fmt.Errorf("resolve primary ref: %w", err)
		}
		powder = input.Powder
		if powder == nil {
			powder = NewPoller(input.Root, cfg.Repo, Scope{})
		}
		if _, err := powder.reconcilePowderPrimary(ctx); err != nil {
			return publishVerdictResult{}, fmt.Errorf("reconcile current Powder Subject before approve: %w", err)
		}
		if err := runConfiguredChecks(ctx, input.Root, revision); err != nil {
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
		"origin",
		checksCommit + ":" + checksRef,
		verdictCommit + ":" + verdictRef,
	}
	if verdict.Verdict == "approve" {
		args = append(args, revision+":"+primaryRef)
	}
	if err := gitRun(ctx, input.Root, args...); err != nil {
		return publishVerdictResult{}, classifyVerdictPush(err)
	}
	result := publishVerdictResult{Status: "published", Revision: revision, Verdict: verdict.Verdict}
	if verdict.Verdict == "approve" {
		reconciliation, reconcileErr := powder.reconcilePowderPrimary(ctx)
		result = withPowderReconciliation(result, reconciliation, reconcileErr)
	}
	return result, nil
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

func identicalEvidence(ctx context.Context, root, checksRef, verdictRef, checksOID, verdictOID string, checksData, verdictData []byte) (bool, error) {
	if checksOID == "" && verdictOID == "" {
		return false, nil
	}
	if checksOID == "" || verdictOID == "" {
		return false, nil
	}
	gotChecks, err := evidenceBlob(ctx, root, checksRef, "checks.json")
	if err != nil {
		return false, err
	}
	gotVerdict, err := evidenceBlob(ctx, root, verdictRef, "verdict.json")
	if err != nil {
		return false, err
	}
	return bytes.Equal(gotChecks, checksData) && bytes.Equal(gotVerdict, verdictData), nil
}

func evidenceBlob(ctx context.Context, root, ref, name string) ([]byte, error) {
	local, err := newPrivateEvidenceRef("refs/forest/private/compare/")
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
