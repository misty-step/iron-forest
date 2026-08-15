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

const reviewRequestAttempts = 3

type publishReviewRequestInput struct {
	Root        string
	Role        string
	Branch      string
	PayloadPath string
	Rejected    string
	RunID       string
}

type publishReviewRequestResult struct {
	Status   string `json:"status"`
	Revision string `json:"revision"`
	Branch   string `json:"branch"`
	Attempts int    `json:"attempts"`
}

func runPublishReviewRequest(rest []string, flags cliFlags) cliOutcome {
	result, err := publishReviewRequest(context.Background(), publishReviewRequestInput{
		Root:        flags.root,
		Role:        rest[0],
		Branch:      rest[1],
		PayloadPath: rest[2],
		Rejected:    flags.rejected,
		RunID:       strings.TrimSpace(os.Getenv("FOREST_RUN_ID")),
	})
	if err != nil {
		if publishConflict(err) {
			return failure(exitConflict, "%s", err)
		}
		return failure(exitError, "%s", err)
	}
	human := fmt.Sprintf("published review-request %s on %s after %d attempt(s)", result.Revision, result.Branch, result.Attempts)
	if result.Status == "identical" {
		human = fmt.Sprintf("accepted identical review-request %s on %s", result.Revision, result.Branch)
	}
	return cliOutcome{Exit: exitOK, Data: result, Human: human}
}

func publishConflict(err error) bool {
	text := err.Error()
	return strings.Contains(text, "conflict") || strings.Contains(text, "branch race") || strings.Contains(text, "canonical note race")
}

func publishReviewRequest(ctx context.Context, input publishReviewRequestInput) (publishReviewRequestResult, error) {
	if input.Role != "builder" && input.Role != "fixer" {
		return publishReviewRequestResult{}, fmt.Errorf("role must be builder or fixer")
	}
	if input.Role == "fixer" && !isSHA(input.Rejected) {
		return publishReviewRequestResult{}, fmt.Errorf("fixer publication requires --rejected <sha>")
	}
	if input.Role == "builder" && input.Rejected != "" {
		return publishReviewRequestResult{}, fmt.Errorf("builder publication does not accept --rejected")
	}
	if input.RunID == "" || strings.ContainsAny(input.RunID, "/ \t\n") {
		return publishReviewRequestResult{}, fmt.Errorf("FOREST_RUN_ID is required")
	}
	payloadPath, err := filepath.Abs(input.PayloadPath)
	if err != nil {
		return publishReviewRequestResult{}, err
	}
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		return publishReviewRequestResult{}, fmt.Errorf("read payload: %w", err)
	}
	payload = bytes.TrimSpace(payload)
	revision, err := gitLine(ctx, input.Root, "rev-parse", "HEAD")
	if err != nil {
		return publishReviewRequestResult{}, err
	}
	note, err := decodeReview(payload, revision)
	if err != nil {
		return publishReviewRequestResult{}, err
	}
	if note.Branch != input.Branch {
		return publishReviewRequestResult{}, fmt.Errorf("payload branch %q does not match %q", note.Branch, input.Branch)
	}
	if err := runConfiguredChecks(ctx, input.Root); err != nil {
		return publishReviewRequestResult{}, err
	}

	baseRef := fmt.Sprintf("refs/notes/forest/private/%s/%s/review-request/%s/base", input.RunID, input.Role, revision)
	privateRef := fmt.Sprintf("refs/notes/forest/private/%s/%s/review-request/%s/publication", input.RunID, input.Role, revision)
	for attempt := 1; attempt <= reviewRequestAttempts; attempt++ {
		noteOID, err := remoteOID(ctx, input.Root, reviewRequestNoteRef)
		if err != nil {
			return publishReviewRequestResult{}, err
		}
		if err := snapshotNoteBase(ctx, input.Root, baseRef, noteOID); err != nil {
			return publishReviewRequestResult{}, err
		}
		destination, destActor, destEmail, err := destinationNote(ctx, input.Root, baseRef, revision)
		if err != nil {
			return publishReviewRequestResult{}, err
		}
		if destination != nil {
			if !bytes.Equal(bytes.TrimSpace(destination), payload) {
				return publishReviewRequestResult{}, fmt.Errorf("conflicting review-request note")
			}
			if destActor != "" && !validIdentity(noteEntry{Author: destActor, Email: destEmail}, "builder", "fixer") {
				return publishReviewRequestResult{}, fmt.Errorf("wrong author identity on review-request")
			}
		}
		branchOID, err := remoteOID(ctx, input.Root, "refs/heads/"+input.Branch)
		if err != nil {
			return publishReviewRequestResult{}, err
		}
		if input.Role == "builder" {
			if branchOID != "" && branchOID != revision {
				return publishReviewRequestResult{}, fmt.Errorf("branch race")
			}
			if branchOID == revision && destination != nil {
				return publishReviewRequestResult{Status: "identical", Revision: revision, Branch: input.Branch, Attempts: attempt}, nil
			}
		} else {
			if branchOID == "" || (branchOID != input.Rejected && branchOID != revision) {
				return publishReviewRequestResult{}, fmt.Errorf("branch race")
			}
			if branchOID == revision && destination != nil {
				return publishReviewRequestResult{Status: "identical", Revision: revision, Branch: input.Branch, Attempts: attempt}, nil
			}
		}
		if err := rebuildPublicationRef(ctx, input.Root, input.Role, privateRef, baseRef, noteOID, revision, payloadPath, destination != nil); err != nil {
			return publishReviewRequestResult{}, err
		}
		pushErr := gitRun(ctx, input.Root, "push", "--atomic", "origin",
			privateRef+":"+reviewRequestNoteRef,
			revision+":refs/heads/"+input.Branch,
		)
		if pushErr == nil {
			return publishReviewRequestResult{Status: "published", Revision: revision, Branch: input.Branch, Attempts: attempt}, nil
		}
		freshBranch, err := remoteOID(ctx, input.Root, "refs/heads/"+input.Branch)
		if err != nil {
			return publishReviewRequestResult{}, err
		}
		freshNote, err := remoteOID(ctx, input.Root, reviewRequestNoteRef)
		if err != nil {
			return publishReviewRequestResult{}, err
		}
		branchReady := (input.Role == "builder" && freshBranch == "") || (input.Role == "fixer" && freshBranch == input.Rejected)
		if !branchReady {
			return publishReviewRequestResult{}, fmt.Errorf("branch race")
		}
		if freshNote == "" || freshNote == noteOID || attempt == reviewRequestAttempts {
			return publishReviewRequestResult{}, fmt.Errorf("canonical note race stopped: %w", pushErr)
		}
	}
	return publishReviewRequestResult{}, fmt.Errorf("canonical note race stopped")
}

func runConfiguredChecks(ctx context.Context, root string) error {
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		return err
	}
	for _, check := range cfg.Checks {
		command := exec.CommandContext(ctx, "sh", "-c", check.Run)
		command.Dir = root
		output, runErr := command.CombinedOutput()
		if runErr != nil {
			return fmt.Errorf("check %q failed: %w\n%s", check.Name, runErr, output)
		}
	}
	return nil
}

func snapshotNoteBase(ctx context.Context, root, baseRef, noteOID string) error {
	if noteOID == "" {
		_ = gitRun(ctx, root, "update-ref", "-d", baseRef)
		return nil
	}
	return gitRun(ctx, root, "fetch", "origin", reviewRequestNoteRef+":"+baseRef)
}

func rebuildPublicationRef(ctx context.Context, root, role, privateRef, baseRef, noteOID, revision, payloadPath string, identical bool) error {
	_ = gitRun(ctx, root, "update-ref", "-d", privateRef)
	if noteOID != "" {
		if err := gitRun(ctx, root, "update-ref", privateRef, baseRef); err != nil {
			return err
		}
	}
	if identical {
		return nil
	}
	name, email := publicationIdentity(role)
	return gitRun(ctx, root, "-c", "user.name="+name, "-c", "user.email="+email, "notes", "--ref="+privateRef, "add", "-F", payloadPath, revision)
}

func publicationIdentity(role string) (string, string) {
	if role == "fixer" {
		return "Iron Forest Fixer", "fixer@forest.invalid"
	}
	return "Iron Forest Builder", "builder@forest.invalid"
}

func destinationNote(ctx context.Context, root, ref, revision string) ([]byte, string, string, error) {
	if _, err := gitOutput(ctx, root, "rev-parse", "--verify", ref); err != nil {
		if soleExitCode(err, 128) {
			return nil, "", "", nil
		}
		return nil, "", "", err
	}
	tree, err := gitOutput(ctx, root, "ls-tree", "-r", ref)
	if err != nil {
		return nil, "", "", err
	}
	var matches []string
	for _, line := range strings.Split(strings.TrimSpace(string(tree)), "\n") {
		if line == "" {
			continue
		}
		modeType, path, ok := strings.Cut(line, "\t")
		if !ok || !strings.Contains(modeType, " blob ") {
			if ok && strings.ReplaceAll(path, "/", "") == revision {
				return nil, "", "", fmt.Errorf("review-request note path is not a blob")
			}
			continue
		}
		if strings.ReplaceAll(path, "/", "") == revision {
			matches = append(matches, path)
		}
	}
	if len(matches) == 0 {
		return nil, "", "", nil
	}
	if len(matches) != 1 {
		return nil, "", "", fmt.Errorf("review-request note path is ambiguous")
	}
	payload, err := gitOutput(ctx, root, "show", ref+":"+matches[0])
	if err != nil {
		return nil, "", "", err
	}
	identity, err := gitLine(ctx, root, "log", "-1", "--format=%an <%ae>", ref, "--", matches[0])
	if err != nil {
		return nil, "", "", err
	}
	name, email, _ := strings.Cut(identity, " <")
	email = strings.TrimSuffix(email, ">")
	return payload, name, email, nil
}

func remoteOID(ctx context.Context, root, ref string) (string, error) {
	output, err := gitOutput(ctx, root, "ls-remote", "origin", ref)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(output))
	if line == "" {
		return "", nil
	}
	oid, _, ok := strings.Cut(line, "\t")
	if !ok {
		oid, _, ok = strings.Cut(line, " ")
	}
	if !ok || !isSHA(oid) {
		return "", fmt.Errorf("malformed ls-remote for %s", ref)
	}
	return oid, nil
}

func gitLine(ctx context.Context, root string, args ...string) (string, error) {
	output, err := gitOutput(ctx, root, args...)
	if err != nil {
		return "", err
	}
	line, err := exactGitLine(output)
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return line, nil
}

func gitRun(ctx context.Context, root string, args ...string) error {
	_, err := gitOutput(ctx, root, args...)
	return err
}

func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}
