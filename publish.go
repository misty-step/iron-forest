package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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

type publishConflictError struct{ err error }

func (e publishConflictError) Error() string { return e.err.Error() }
func (e publishConflictError) Unwrap() error { return e.err }

func conflictError(format string, args ...any) error {
	return publishConflictError{err: fmt.Errorf(format, args...)}
}

func publishConflict(err error) bool {
	var conflict publishConflictError
	return errors.As(err, &conflict)
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
	revision, err := gitLine(ctx, input.Root, "rev-parse", "HEAD")
	if err != nil {
		return publishReviewRequestResult{}, err
	}
	note, err := decodeReview(payload, revision)
	if err != nil {
		return publishReviewRequestResult{}, err
	}
	if note.Tracker != "github" && note.Tracker != "powder" {
		return publishReviewRequestResult{}, fmt.Errorf("review-request tracker must be github or powder")
	}
	if note.Branch != input.Branch {
		return publishReviewRequestResult{}, fmt.Errorf("payload branch %q does not match %q", note.Branch, input.Branch)
	}
	if err := runConfiguredChecks(ctx, input.Root, revision); err != nil {
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
			if !bytes.Equal(destination, payload) {
				return publishReviewRequestResult{}, conflictError("conflicting review-request note")
			}
			if !validIdentity(noteEntry{Author: destActor, Email: destEmail}, "builder", "fixer") {
				return publishReviewRequestResult{}, conflictError("wrong author identity on review-request")
			}
		}
		branchOID, err := remoteOID(ctx, input.Root, "refs/heads/"+input.Branch)
		if err != nil {
			return publishReviewRequestResult{}, err
		}
		if input.Role == "builder" {
			if branchOID != "" && (branchOID != revision || destination == nil) {
				return publishReviewRequestResult{}, conflictError("branch race")
			}
			if branchOID == revision && destination != nil {
				return publishReviewRequestResult{Status: "identical", Revision: revision, Branch: input.Branch, Attempts: attempt}, nil
			}
		} else {
			if branchOID == "" || (branchOID == revision && destination == nil) || (branchOID != input.Rejected && branchOID != revision) {
				return publishReviewRequestResult{}, conflictError("branch race")
			}
			if branchOID == revision && destination != nil {
				return publishReviewRequestResult{Status: "identical", Revision: revision, Branch: input.Branch, Attempts: attempt}, nil
			}
		}
		if err := rebuildPublicationRef(ctx, input.Root, input.Role, privateRef, baseRef, noteOID, revision, payload, destination != nil); err != nil {
			return publishReviewRequestResult{}, err
		}
		expectedNote := noteOID
		if expectedNote == "" {
			expectedNote = strings.Repeat("0", 40)
		}
		expectedBranch := strings.Repeat("0", 40)
		if input.Role == "fixer" {
			expectedBranch = input.Rejected
		}
		requestRef := evidenceRequestRefPrefix + revision
		existingRequest, err := remoteOID(ctx, input.Root, requestRef)
		if err != nil {
			return publishReviewRequestResult{}, err
		}
		if existingRequest != "" {
			got, err := evidenceBlob(ctx, input.Root, requestRef, "request.json")
			if err != nil {
				return publishReviewRequestResult{}, err
			}
			if !bytes.Equal(got, payload) {
				return publishReviewRequestResult{}, conflictError("conflicting request evidence for %s", revision)
			}
		}
		pushArgs := []string{
			"push", "--atomic",
			"--force-with-lease=" + reviewRequestNoteRef + ":" + expectedNote,
			"--force-with-lease=refs/heads/" + input.Branch + ":" + expectedBranch,
		}
		if existingRequest == "" {
			name, email := publicationIdentity(input.Role)
			requestCommit, err := commitEvidenceAs(ctx, input.Root, "request.json", payload, "forest request "+revision, name, email)
			if err != nil {
				return publishReviewRequestResult{}, err
			}
			pushArgs = append(pushArgs, "--force-with-lease="+requestRef+":")
			pushArgs = append(pushArgs, "origin", privateRef+":"+reviewRequestNoteRef, revision+":refs/heads/"+input.Branch, requestCommit+":"+requestRef)
		} else {
			pushArgs = append(pushArgs, "origin", privateRef+":"+reviewRequestNoteRef, revision+":refs/heads/"+input.Branch)
		}
		pushErr := gitRun(ctx, input.Root, pushArgs...)

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
		branchUnchanged := (input.Role == "builder" && freshBranch == "") || (input.Role == "fixer" && freshBranch == input.Rejected)
		if freshBranch == revision {
			if err := snapshotNoteBase(ctx, input.Root, baseRef, freshNote); err != nil {
				return publishReviewRequestResult{}, err
			}
			destination, destActor, destEmail, err := destinationNote(ctx, input.Root, baseRef, revision)
			if err != nil {
				return publishReviewRequestResult{}, err
			}
			if destination != nil && bytes.Equal(destination, payload) && validIdentity(noteEntry{Author: destActor, Email: destEmail}, "builder", "fixer") {
				return publishReviewRequestResult{Status: "identical", Revision: revision, Branch: input.Branch, Attempts: attempt}, nil
			}
			return publishReviewRequestResult{}, conflictError("canonical note race stopped: %w", pushErr)
		}
		if !branchUnchanged {
			return publishReviewRequestResult{}, conflictError("branch race")
		}
		if freshNote == "" || freshNote == noteOID || attempt == reviewRequestAttempts {
			return publishReviewRequestResult{}, conflictError("canonical note race stopped: %w", pushErr)
		}
	}
	return publishReviewRequestResult{}, conflictError("canonical note race stopped")
}

func runConfiguredChecks(ctx context.Context, root, revision string) (err error) {
	shell, err := trustedExecutable(root, "sh")
	if err != nil {
		return err
	}
	primary, err := primaryCheckout(ctx, root)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(forestPath(primary, "worktrees"), 0o755); err != nil {
		return err
	}
	dir := forestPath(primary, "worktrees", newRunID("checks", time.Now()))
	if addErr := gitRun(ctx, root, "worktree", "add", "--detach", dir, revision); addErr != nil {
		return errors.Join(addErr, os.RemoveAll(dir))
	}
	defer func() {
		err = errors.Join(err, removePublishWorktree(root, dir))
	}()
	cfg, loadErr := loadConfig(configPath(dir))
	if loadErr != nil {
		return loadErr
	}
	path, pathErr := trustedPath(root)
	if pathErr != nil {
		return pathErr
	}
	for _, check := range cfg.Checks {
		command := exec.CommandContext(ctx, shell, "-c", check.Run)
		command.Dir = dir
		command.Env = checkEnvironment(path)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, runErr := processGroupOutput(ctx, command)
		if runErr != nil {
			return fmt.Errorf("check %q failed: %w\n%s%s", check.Name, runErr, output, stderr.Bytes())
		}
	}
	return nil
}

func checkEnvironment(path string) []string {
	environment := os.Environ()
	replaced := false
	for index, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if name == "PATH" {
			environment[index] = "PATH=" + path
			replaced = true
		}
	}
	if !replaced {
		environment = append(environment, "PATH="+path)
	}
	return environment
}

func primaryCheckout(ctx context.Context, root string) (string, error) {
	output, err := gitOutput(ctx, root, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(output), "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if ok && path != "" {
			return path, nil
		}
	}
	return "", fmt.Errorf("primary checkout is unknown")
}

func removePublishWorktree(root, dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	removeErr := gitRun(ctx, root, "worktree", "remove", "--force", dir)
	if removeErr != nil {
		removeErr = fmt.Errorf("git worktree remove: %w", removeErr)
	}
	filesystemErr := os.RemoveAll(dir)
	if filesystemErr != nil {
		filesystemErr = fmt.Errorf("remove worktree path: %w", filesystemErr)
	}
	pruneErr := gitRun(ctx, root, "worktree", "prune", "--expire=now")
	if pruneErr != nil {
		pruneErr = fmt.Errorf("git worktree prune: %w", pruneErr)
	}
	return errors.Join(removeErr, filesystemErr, pruneErr)
}

func snapshotNoteBase(ctx context.Context, root, baseRef, noteOID string) error {
	if noteOID == "" {
		return gitRun(ctx, root, "update-ref", "-d", baseRef)
	}
	return gitRun(ctx, root, "fetch", "origin", reviewRequestNoteRef+":"+baseRef)
}

func rebuildPublicationRef(ctx context.Context, root, role, privateRef, baseRef, noteOID, revision string, payload []byte, identical bool) error {
	if err := gitRun(ctx, root, "update-ref", "-d", privateRef); err != nil {
		return err
	}
	if noteOID != "" {
		if err := gitRun(ctx, root, "update-ref", privateRef, baseRef); err != nil {
			return err
		}
	}
	if identical {
		return nil
	}
	name, email := publicationIdentity(role)
	return gitNotesAdd(ctx, root, name, email, privateRef, revision, payload)
}

func gitNotesAdd(ctx context.Context, root, name, email, ref, revision string, payload []byte) error {
	path, err := trustedExecutable(root, "git")
	if err != nil {
		return err
	}
	command := exec.CommandContext(ctx, path, "-C", root, "notes", "--ref="+ref, "add", "-F", "-", revision)
	command.Stdin = bytes.NewReader(payload)
	command.Env = publicationGitEnv(name, email)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("git notes add: %w\n%s", err, stderr.Bytes())
	}
	return nil
}

func publicationGitEnv(name, email string) []string {
	environment := childEnvironment()
	return append(environment,
		"GIT_AUTHOR_NAME="+name,
		"GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME="+name,
		"GIT_COMMITTER_EMAIL="+email,
	)
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
	name, email := parseNoteIdentity(identity)
	return payload, name, email, nil
}

func parseNoteIdentity(identity string) (string, string) {
	name, email, ok := strings.Cut(identity, " <")
	if !ok {
		return "", ""
	}
	return name, strings.TrimSuffix(email, ">")
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

func gitInput(ctx context.Context, root string, input []byte, args ...string) error {
	_, err := gitOutputInput(ctx, root, input, args...)
	return err
}

func gitOutput(ctx context.Context, root string, args ...string) ([]byte, error) {
	return gitOutputInput(ctx, root, nil, args...)
}

func gitOutputInput(ctx context.Context, root string, input []byte, args ...string) ([]byte, error) {
	path, err := trustedExecutable(root, "git")
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, path, append([]string{"-C", root}, args...)...)
	if input != nil {
		command.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}
