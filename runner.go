package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Runner struct {
	Root    string
	GitPath string
	OMPPath string
}

func NewRunner(root string) *Runner {
	return &Runner{Root: root, GitPath: "git", OMPPath: "omp"}
}

func (r *Runner) Run(ctx context.Context, declaration Declaration, timeoutSeconds int) (RunRecord, error) {
	timeout, err := durationFromSeconds(timeoutSeconds)
	if err != nil {
		return RunRecord{}, err
	}
	started := time.Now().UTC()
	runID := newRunID(declaration.Name, started)
	record := RunRecord{RunID: runID, Agent: declaration.Name, Started: started.Format(time.RFC3339Nano)}
	logPath := forestPath(r.Root, "runs", runID+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return record, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return record, err
	}
	defer logFile.Close()

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	worktree := forestPath(r.Root, "worktrees", runID)
	worktreeMayExist, prepareErr := r.prepareWorktree(runCtx, worktree)
	if prepareErr != nil {
		var cleanupErr error
		if worktreeMayExist {
			cleanupErr = r.cleanupWorktree(worktree)
			if cleanupErr != nil {
				cleanupErr = fmt.Errorf("cleanup worktree: %w", cleanupErr)
				_, _ = fmt.Fprintln(logFile, cleanupErr)
			}
		}
		record.Exit = runContextExit(ctx, runCtx, 1)
		record.Duration = time.Now().Sub(started).Seconds()
		_, _ = fmt.Fprintf(logFile, "prepare worktree: %v\n", prepareErr)
		appendErr := AppendRun(r.Root, record)
		return record, errors.Join(prepareErr, cleanupErr, appendErr)
	}

	invokeErr := r.invoke(runCtx, worktree, declaration, timeoutSeconds, logFile, &record)
	cleanupErr := r.cleanupWorktree(worktree)
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("cleanup worktree: %w", cleanupErr)
		_, _ = fmt.Fprintln(logFile, cleanupErr)
		if record.Exit == 0 {
			record.Exit = 1
		}
	}
	record.Duration = time.Now().Sub(started).Seconds()
	usage, usageErr := parseOMPUsage(logPath)
	if usageErr != nil {
		usageErr = fmt.Errorf("parse OMP usage: %w", usageErr)
		if record.Exit == 0 {
			record.Exit = 1
		}
		_, _ = fmt.Fprintln(logFile, usageErr)
	} else {
		record.TokensIn, record.TokensOut = usage.TokensIn, usage.TokensOut
		record.CacheRead, record.CacheWrite = usage.CacheRead, usage.CacheWrite
		record.Reasoning = usage.Reasoning
	}
	appendErr := AppendRun(r.Root, record)
	if err := errors.Join(invokeErr, cleanupErr, usageErr, appendErr); err != nil {
		return record, err
	}
	if record.Exit != 0 {
		return record, fmt.Errorf("agent %s exited with %d", declaration.Name, record.Exit)
	}
	return record, nil
}

func newRunID(agent string, now time.Time) string {
	agent = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, agent)
	return fmt.Sprintf("%d-%s", now.UnixNano(), agent)
}

func (r *Runner) prepareWorktree(ctx context.Context, path string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	if _, err := r.git(ctx, r.Root, "fetch", "origin", "master"); err != nil {
		return false, fmt.Errorf("fetch origin/master: %w", err)
	}
	if _, err := r.git(ctx, r.Root, "worktree", "add", "--detach", path, "origin/master"); err != nil {
		return true, fmt.Errorf("add worktree: %w", err)
	}
	return true, nil
}

const (
	cleanupTimeout                    = 10 * time.Second
	cleanupRemoveExecutionTimeout     = 2 * time.Second
	cleanupFilesystemExecutionTimeout = time.Second
	cleanupPruneExecutionTimeout      = time.Second
	cleanupReservedSlack              = cleanupTimeout -
		cleanupRemoveExecutionTimeout -
		cleanupFilesystemExecutionTimeout -
		cleanupPruneExecutionTimeout -
		3*processStopWorstCase
)

func (r *Runner) cleanupWorktree(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return r.removeWorktree(ctx, path)
}

func (r *Runner) removeWorktree(ctx context.Context, path string) error {
	removeCtx, cancelRemove := context.WithTimeout(ctx, cleanupRemoveExecutionTimeout)
	_, removeErr := r.git(removeCtx, r.Root, "worktree", "remove", "--force", path)
	cancelRemove()
	if removeErr != nil {
		removeErr = fmt.Errorf("git worktree remove: %w", removeErr)
	}
	filesystemCtx, cancelFilesystem := context.WithTimeout(ctx, cleanupFilesystemExecutionTimeout)
	filesystemErr := r.removeFilesystem(filesystemCtx, path)
	cancelFilesystem()
	if filesystemErr != nil {
		filesystemErr = fmt.Errorf("remove worktree path: %w", filesystemErr)
	}
	pruneCtx, cancelPrune := context.WithTimeout(ctx, cleanupPruneExecutionTimeout)
	_, pruneErr := r.git(pruneCtx, r.Root, "worktree", "prune", "--expire=now")
	cancelPrune()
	if pruneErr != nil {
		pruneErr = fmt.Errorf("git worktree prune: %w", pruneErr)
	}
	return errors.Join(removeErr, filesystemErr, pruneErr)
}

func (r *Runner) removeFilesystem(ctx context.Context, path string) error {
	executable, err := trustedExecutable(r.Root, "rm")
	if err != nil {
		return err
	}
	_, err = processGroupOutput(ctx, exec.Command(executable, "-rf", "--", path))
	return err
}

func (r *Runner) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	path, err := trustedExecutable(r.Root, r.GitPath)
	if err != nil {
		return nil, err
	}
	command := exec.Command(path, args...)
	command.Dir = dir
	return processGroupOutput(ctx, command)
}

func processGroupOutput(ctx context.Context, command *exec.Cmd) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(ctx.Err(), err)
	}
	command.Stdout = writer
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		return nil, errors.Join(ctx.Err(), err, writer.Close(), reader.Close())
	}
	writerCloseErr := writer.Close()
	var output bytes.Buffer
	read := make(chan error, 1)
	go func() {
		_, err := io.Copy(&output, reader)
		read <- err
	}()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	var runErr, cleanupErr error
	select {
	case runErr = <-wait:
		cleanupErr = stopResidualProcessGroup(command.Process.Pid)
		if contextErr := ctx.Err(); contextErr != nil {
			runErr = errors.Join(contextErr, runErr)
		}
	case <-ctx.Done():
		runErr = ctx.Err()
		cleanupErr = stopProcessGroup(command.Process.Pid, wait)
	}
	var readerCloseErr error
	if cleanupErr != nil {
		readerCloseErr = reader.Close()
	}
	readErr := <-read
	if cleanupErr == nil {
		readerCloseErr = reader.Close()
	}
	return output.Bytes(), errors.Join(runErr, cleanupErr, writerCloseErr, readErr, readerCloseErr)
}

func (r *Runner) invoke(ctx context.Context, worktree string, declaration Declaration, timeoutSeconds int, logFile *os.File, record *RunRecord) error {
	if err := ctx.Err(); err != nil {
		record.Exit = contextExit(err)
		return err
	}
	path, err := r.ompExecutable()
	if err != nil {
		record.Exit = 127
		return err
	}
	args := []string{"-p", "--mode", "json", "--no-session", "--auto-approve", "--cwd", worktree, "--max-time", strconv.Itoa(timeoutSeconds), "--model", declaration.Model, "--system-prompt", declaration.SystemPrompt}
	if len(declaration.Tools) > 0 {
		args = append(args, "--tools", strings.Join(declaration.Tools, ","))
	}
	if declaration.Thinking != "" {
		args = append(args, "--thinking", declaration.Thinking)
	}
	args = append(args, declaration.TaskPrompt)
	command := exec.Command(path, args...)
	command.Dir = worktree
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	name := "Iron Forest " + strings.ToUpper(declaration.Name[:1]) + declaration.Name[1:]
	email := declaration.Name + "@forest.invalid"
	command.Env, err = runnerEnvironment(r.Root, name, email)
	if err != nil {
		record.Exit = 127
		return err
	}
	if err := command.Start(); err != nil {
		record.Exit = 127
		return fmt.Errorf("start omp: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	select {
	case err := <-wait:
		var cleanupErr error
		record.Exit, err = processResult(ctx, err)
		cleanupErr = stopResidualProcessGroup(command.Process.Pid)
		if cleanupErr != nil && record.Exit == 0 {
			record.Exit = 1
		}
		return errors.Join(err, cleanupErr)
	case <-ctx.Done():
		cleanupErr := stopProcessGroup(command.Process.Pid, wait)
		record.Exit = contextExit(ctx.Err())
		if record.Exit == 124 {
			_, _ = fmt.Fprintln(logFile, "omp wall-clock timeout")
			return errors.Join(fmt.Errorf("omp timed out after %ds", timeoutSeconds), cleanupErr)
		}
		return errors.Join(ctx.Err(), cleanupErr)
	}
}

const (
	processStopGrace       = time.Second
	processGroupProbeLimit = 250 * time.Millisecond
	processGroupProbeStep  = 10 * time.Millisecond
	processStopWorstCase   = processStopGrace + 2*processGroupProbeLimit
)

func stopProcessGroup(pid int, wait <-chan error) error {
	if processGroupExists(pid) {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	timer := time.NewTimer(processStopGrace)
	defer timer.Stop()
	select {
	case <-wait:
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		waitTimer := time.NewTimer(processGroupProbeLimit)
		defer waitTimer.Stop()
		select {
		case <-wait:
		case <-waitTimer.C:
		}
	}
	if processGroupExists(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	if !waitProcessGroupQuiescence(pid, processGroupProbeLimit) {
		return fmt.Errorf("process group %d did not quiesce", pid)
	}
	return nil
}

func stopResidualProcessGroup(pid int) error {
	if !processGroupExists(pid) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if waitProcessGroupQuiescence(pid, processStopGrace) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	if !waitProcessGroupQuiescence(pid, processGroupProbeLimit) {
		return fmt.Errorf("process group %d did not quiesce", pid)
	}
	return nil
}

func processGroupExists(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitProcessGroupQuiescence(pid int, limit time.Duration) bool {
	if !processGroupExists(pid) {
		return true
	}
	timer := time.NewTimer(limit)
	defer timer.Stop()
	ticker := time.NewTicker(processGroupProbeStep)
	defer ticker.Stop()
	for {
		select {
		case <-timer.C:
			return !processGroupExists(pid)
		case <-ticker.C:
			if !processGroupExists(pid) {
				return true
			}
		}
	}
}

func processResult(ctx context.Context, err error) (int, error) {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextExit(contextErr), contextErr
	}
	exit := processExit(err)
	if err != nil && exit == 0 {
		exit = 1
	}
	return exit, nil
}

func contextExit(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return 124
	}
	return 130
}

func runContextExit(parent, run context.Context, fallback int) int {
	if err := run.Err(); err != nil {
		return contextExit(err)
	}
	if err := parent.Err(); err != nil {
		return contextExit(err)
	}
	return fallback
}

func (r *Runner) ompExecutable() (string, error) {
	if r.OMPPath != "omp" {
		return trustedExecutable(r.Root, r.OMPPath)
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".local", "bin", "omp")
		if _, err := os.Stat(candidate); err == nil {
			return trustedExecutable(r.Root, candidate)
		}
	}
	return trustedExecutable(r.Root, "omp")
}

func trustedExecutable(root, name string) (string, error) {
	path := name
	if !strings.ContainsRune(path, os.PathSeparator) {
		found, err := exec.LookPath(path)
		if err != nil {
			return "", err
		}
		path = found
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved := absolute
	if value, err := filepath.EvalSymlinks(absolute); err == nil {
		resolved = value
	}
	inside, err := pathInside(root, resolved)
	if err != nil {
		return "", err
	}
	if inside {
		return "", fmt.Errorf("refuse repository executable %s", resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s is not executable", resolved)
	}
	return resolved, nil
}

func runnerEnvironment(root, name, email string) ([]string, error) {
	path, err := trustedPath(root)
	if err != nil {
		return nil, err
	}
	blocked := []string{"PATH=", "GIT_AUTHOR_NAME=", "GIT_AUTHOR_EMAIL=", "GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL="}
	environment := make([]string, 0, len(os.Environ())+5)
	for _, value := range os.Environ() {
		keep := true
		for _, prefix := range blocked {
			if strings.HasPrefix(value, prefix) {
				keep = false
				break
			}
		}
		if keep {
			environment = append(environment, value)
		}
	}
	return append(environment,
		"PATH="+path,
		"GIT_AUTHOR_NAME="+name,
		"GIT_AUTHOR_EMAIL="+email,
		"GIT_COMMITTER_NAME="+name,
		"GIT_COMMITTER_EMAIL="+email,
	), nil
}

func trustedPath(root string) (string, error) {
	entries := make([]string, 0)
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if entry == "" {
			continue
		}
		absolute, err := filepath.Abs(entry)
		if err != nil {
			return "", err
		}
		resolved := absolute
		if value, err := filepath.EvalSymlinks(absolute); err == nil {
			resolved = value
		}
		inside, err := pathInside(root, resolved)
		if err != nil {
			return "", err
		}
		if !inside {
			entries = append(entries, resolved)
		}
	}
	if len(entries) == 0 {
		return "", fmt.Errorf("PATH has no trusted directories")
	}
	return strings.Join(entries, string(os.PathListSeparator)), nil
}

func pathInside(root, path string) (bool, error) {
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	if value, err := filepath.EvalSymlinks(rootPath); err == nil {
		rootPath = value
	}
	relative, err := filepath.Rel(rootPath, path)
	if err != nil {
		return false, err
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator)), nil
}

func processExit(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		return exitErr.ProcessState.ExitCode()
	}
	return 1
}

func parseOMPUsage(path string) (Usage, error) {
	file, err := os.Open(path)
	if err != nil {
		return Usage{}, err
	}
	defer file.Close()
	var latest, total Usage
	found := false
	turns := false
	lineNumber := 0
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) != 0 {
			lineNumber++
			var value any
			decoder := json.NewDecoder(bytes.NewReader(line))
			decoder.UseNumber()
			decoded := decoder.Decode(&value) == nil
			if decoded {
				var extra any
				decoded = errors.Is(decoder.Decode(&extra), io.EOF)
			}
			if decoded {
				usage, ok, usageErr := findUsage(value)
				if usageErr != nil {
					return Usage{}, fmt.Errorf("OMP usage line %d: %w", lineNumber, usageErr)
				}
				if ok {
					latest = usage
					found = true
				}
				if object, ok := value.(map[string]any); ok && object["type"] == "turn_end" {
					usage, ok, usageErr = findUsage(object["message"])
					if usageErr != nil {
						return Usage{}, fmt.Errorf("OMP usage line %d: %w", lineNumber, usageErr)
					}
					if ok {
						total, usageErr = addUsage(total, usage)
						if usageErr != nil {
							return Usage{}, fmt.Errorf("OMP usage line %d: %w", lineNumber, usageErr)
						}
						turns = true
					}
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return Usage{}, readErr
		}
	}
	if turns {
		return total, nil
	}
	if !found {
		return Usage{}, fmt.Errorf("OMP output has no usage")
	}
	return latest, nil
}

func findUsage(value any) (Usage, bool, error) {
	switch object := value.(type) {
	case map[string]any:
		if raw, ok := object["usage"].(map[string]any); ok {
			usage, err := usageFromMap(raw)
			return usage, true, err
		}
		for _, nested := range object {
			usage, ok, err := findUsage(nested)
			if ok || err != nil {
				return usage, ok, err
			}
		}
	case []any:
		for _, nested := range object {
			usage, ok, err := findUsage(nested)
			if ok || err != nil {
				return usage, ok, err
			}
		}
	}
	return Usage{}, false, nil
}

func addUsage(left, right Usage) (Usage, error) {
	var err error
	if left.TokensIn, err = checkedUsageSum("input", left.TokensIn, right.TokensIn); err != nil {
		return Usage{}, err
	}
	if left.TokensOut, err = checkedUsageSum("output", left.TokensOut, right.TokensOut); err != nil {
		return Usage{}, err
	}
	if left.CacheRead, err = checkedUsageSum("cache read", left.CacheRead, right.CacheRead); err != nil {
		return Usage{}, err
	}
	if left.CacheWrite, err = checkedUsageSum("cache write", left.CacheWrite, right.CacheWrite); err != nil {
		return Usage{}, err
	}
	if left.Reasoning, err = checkedUsageSum("reasoning", left.Reasoning, right.Reasoning); err != nil {
		return Usage{}, err
	}
	return left, nil
}

func checkedUsageSum(name string, left, right int64) (int64, error) {
	const maxInt64 = int64(^uint64(0) >> 1)
	if left > maxInt64-right {
		return 0, fmt.Errorf("%s usage overflow", name)
	}
	return left + right, nil
}

func usageFromMap(value map[string]any) (Usage, error) {
	var usage Usage
	var err error
	recognized := false
	var present bool
	if usage.TokensIn, present, err = number(value, "input", "tokens_in", "input_tokens"); err != nil {
		return Usage{}, err
	}
	recognized = recognized || present
	if usage.TokensOut, present, err = number(value, "output", "tokens_out", "output_tokens"); err != nil {
		return Usage{}, err
	}
	recognized = recognized || present
	if usage.CacheRead, present, err = number(value, "cacheRead", "cache_read"); err != nil {
		return Usage{}, err
	}
	recognized = recognized || present
	if usage.CacheWrite, present, err = number(value, "cacheWrite", "cache_write"); err != nil {
		return Usage{}, err
	}
	recognized = recognized || present
	if usage.Reasoning, present, err = number(value, "reasoning", "reasoningTokens", "reasoning_tokens"); err != nil {
		return Usage{}, err
	}
	recognized = recognized || present
	if !recognized {
		return Usage{}, fmt.Errorf("usage object has no recognized alias")
	}
	return usage, nil
}

func number(value map[string]any, keys ...string) (int64, bool, error) {
	for _, key := range keys {
		raw, ok := value[key]
		if !ok {
			continue
		}
		n, ok := raw.(json.Number)
		if !ok {
			return 0, true, fmt.Errorf("%s usage must be an exact nonnegative int64", key)
		}
		parsed, err := n.Int64()
		if err != nil || parsed < 0 {
			return 0, true, fmt.Errorf("%s usage must be an exact nonnegative int64, got %q", key, n)
		}
		return parsed, true, nil
	}
	return 0, false, nil
}
