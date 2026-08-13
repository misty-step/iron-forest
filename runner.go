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
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Runner struct {
	Root    string
	GitPath string
	PiPath  string
}

const (
	trustedTransportOutputLimit = 1 << 20
	runLogHalfLimit             = 1 << 20
	completedRunLogRetention    = 32
	runLogTruncationMarker      = "\n--- Iron Forest Run log truncated; retained first 1 MiB and last 1 MiB ---\n"
	// harnessUnavailableExit is the shell convention for a command that could not
	// be executed. A Run that never started has no usage to report.
	harnessUnavailableExit = 127
)

var (
	errTrustedTransportOutputOverflow = errors.New("trusted transport output exceeded 1 MiB")
	runLogRegistry                    = struct {
		sync.Mutex
		active map[string]struct{}
	}{active: make(map[string]struct{})}
)

type boundedTransportOutput struct {
	data     []byte
	overflow bool
}

func (output *boundedTransportOutput) Write(data []byte) (int, error) {
	size := len(data)
	retained := min(size, trustedTransportOutputLimit-len(output.data))
	output.data = append(output.data, data[:retained]...)
	output.overflow = output.overflow || retained != size
	return size, nil
}

func (output *boundedTransportOutput) err() error {
	if output.overflow {
		return errTrustedTransportOutputOverflow
	}
	return nil
}

type boundedRunLog struct {
	file      *os.File
	first     int
	tail      []byte
	tailLen   int
	tailNext  int
	truncated bool
	writeErr  error
}

func openBoundedRunLog(path string) (*boundedRunLog, error) {
	runLogRegistry.Lock()
	defer runLogRegistry.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	log := &boundedRunLog{file: file, tail: make([]byte, runLogHalfLimit)}
	runLogRegistry.active[path] = struct{}{}
	return log, nil
}

func (log *boundedRunLog) Write(data []byte) (int, error) {
	size := len(data)
	if remaining := runLogHalfLimit - log.first; remaining > 0 {
		retained := min(len(data), remaining)
		log.writeFile(data[:retained])
		log.first += retained
		data = data[retained:]
	}
	log.retainTail(data)
	return size, nil
}

func (log *boundedRunLog) retainTail(data []byte) {
	for len(data) > 0 {
		if log.tailLen < len(log.tail) {
			retained := copy(log.tail[log.tailLen:], data)
			log.tailLen += retained
			data = data[retained:]
			if len(data) == 0 {
				return
			}
		}
		log.truncated = true
		retained := copy(log.tail[log.tailNext:], data)
		log.tailNext = (log.tailNext + retained) % len(log.tail)
		data = data[retained:]
	}
}

func (log *boundedRunLog) writeFile(data []byte) {
	written, err := log.file.Write(data)
	if written != len(data) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	log.writeErr = errors.Join(log.writeErr, err)
}

func (log *boundedRunLog) Finalize() error {
	if log.truncated {
		log.writeFile([]byte(runLogTruncationMarker))
		log.writeFile(log.tail[log.tailNext:])
		log.writeFile(log.tail[:log.tailNext])
	} else {
		log.writeFile(log.tail[:log.tailLen])
	}
	return errors.Join(log.writeErr, log.file.Close())
}

func completeRunLog(path string) error {
	runLogRegistry.Lock()
	defer runLogRegistry.Unlock()
	delete(runLogRegistry.active, path)
	return pruneCompletedRunLogs(filepath.Dir(path))
}

type completedRunLog struct {
	name    string
	path    string
	modTime time.Time
}

func pruneCompletedRunLogs(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	logs := make([]completedRunLog, 0, completedRunLogRetention)
	var pruneErr error
	for {
		entries, readErr := directory.ReadDir(64)
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			if _, active := runLogRegistry.active[path]; active || !isReservedRunLogName(entry.Name()) {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				pruneErr = errors.Join(pruneErr, err)
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			current := completedRunLog{name: entry.Name(), path: path, modTime: info.ModTime()}
			if len(logs) < completedRunLogRetention {
				logs = append(logs, current)
				continue
			}
			oldest := 0
			for offset, candidate := range logs[1:] {
				if newerRunLog(logs[oldest], candidate) {
					oldest = offset + 1
				}
			}
			remove := current
			if newerRunLog(current, logs[oldest]) {
				remove, logs[oldest] = logs[oldest], current
			}
			if err := os.Remove(remove.path); err != nil {
				pruneErr = errors.Join(pruneErr, err)
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				pruneErr = errors.Join(pruneErr, readErr)
			}
			break
		}
	}
	return errors.Join(pruneErr, directory.Close())
}

func newerRunLog(left, right completedRunLog) bool {
	if left.modTime.Equal(right.modTime) {
		return left.name > right.name
	}
	return left.modTime.After(right.modTime)
}

func isReservedRunLogName(name string) bool {
	if !strings.HasSuffix(name, ".log") {
		return false
	}
	return isReservedRunID(strings.TrimSuffix(name, ".log"))
}

// runLogPath names the per-Run log. The Runner owns this layout; readers use
// this helper so the name is stated once.
func runLogPath(root, runID string) string {
	return forestPath(root, "runs", runID+".log")
}

func NewRunner(root string) *Runner {
	return &Runner{Root: root, GitPath: "git", PiPath: "pi"}
}

func (r *Runner) Run(ctx context.Context, declaration Declaration, timeoutSeconds int) (RunRecord, error) {
	timeout, err := durationFromSeconds(timeoutSeconds)
	if err != nil {
		return RunRecord{}, err
	}
	started := time.Now().UTC()
	runID := newRunID(declaration.Name, started)
	record := RunRecord{RunID: runID, Agent: declaration.Name, Started: started.Format(time.RFC3339Nano)}
	logPath := runLogPath(r.Root, runID)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return record, err
	}
	logFile, err := openBoundedRunLog(logPath)
	if err != nil {
		return record, err
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	worktree := forestPath(r.Root, "worktrees", runID)
	worktreeMayExist, prepareErr := r.prepareWorktree(runCtx, worktree)
	if prepareErr != nil {
		var cleanupErr error
		if worktreeMayExist {
			cleanupErr = r.cleanupWorktree(worktree, runID)
			if cleanupErr != nil {
				cleanupErr = fmt.Errorf("cleanup worktree: %w", cleanupErr)
				_, _ = fmt.Fprintln(logFile, cleanupErr)
			}
		}
		record.Exit = runContextExit(ctx, runCtx, 1)
		record.Duration = time.Since(started).Seconds()
		_, _ = fmt.Fprintf(logFile, "prepare worktree: %v\n", prepareErr)
		finalizeErr := logFile.Finalize()
		if finalizeErr != nil {
			finalizeErr = fmt.Errorf("finalize Run log: %w", finalizeErr)
		}
		retentionErr := completeRunLog(logPath)
		if retentionErr != nil {
			retentionErr = fmt.Errorf("retain completed Run logs: %w", retentionErr)
		}
		appendErr := AppendRun(r.Root, record)
		return record, errors.Join(prepareErr, cleanupErr, finalizeErr, retentionErr, appendErr)
	}

	// The profile is materialized after the worktree exists, because its evidence
	// belongs to a Run that really started. A failure here means the harness never
	// ran, which the usage parser must know.
	defaults, _, err := loadDefaults(r.Root)
	var profileDir string
	var profileFiles []string
	var profileErr error
	if err != nil {
		profileErr = fmt.Errorf("load instance defaults: %w", err)
	} else {
		profileDir, profileFiles, profileErr = materializeRunProfile(r.Root, runID, declaration, defaults)
	}
	harnessRunnable := profileErr == nil
	if profileErr != nil {
		record.Exit = 1
		_, _ = fmt.Fprintf(logFile, "materialize Run profile: %v\n", profileErr)
	} else {
		_, _ = fmt.Fprintln(logFile, runEvidenceLine(record, declaration, profileFiles))
	}
	var invokeErr error
	if harnessRunnable {
		invokeErr = r.invoke(runCtx, worktree, declaration, profileDir, timeoutSeconds, logFile, &record)
	}
	cleanupErr := r.cleanupWorktree(worktree, runID)
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("cleanup worktree: %w", cleanupErr)
		_, _ = fmt.Fprintln(logFile, cleanupErr)
		if record.Exit == 0 {
			record.Exit = 1
		}
	}
	if profileErr := collectRunProfile(r.Root, runID); profileErr != nil {
		profileErr = fmt.Errorf("collect Run profile: %w", profileErr)
		_, _ = fmt.Fprintln(logFile, profileErr)
		cleanupErr = errors.Join(cleanupErr, profileErr)
		if record.Exit == 0 {
			record.Exit = 1
		}
	}
	record.Duration = time.Since(started).Seconds()
	finalizeErr := logFile.Finalize()
	if finalizeErr != nil {
		finalizeErr = fmt.Errorf("finalize Run log: %w", finalizeErr)
		if record.Exit == 0 {
			record.Exit = 1
		}
	}
	// Usage exists only if the harness ran. Demanding it after a failure to start
	// would report "no usage" as the cause and bury the real one.
	var usageErr error
	if harnessRunnable && (invokeErr == nil || record.Exit != harnessUnavailableExit) {
		usage, parseErr := parseAgentUsage(logPath)
		if parseErr != nil {
			usageErr = fmt.Errorf("parse harness usage: %w", parseErr)
			if record.Exit == 0 {
				record.Exit = 1
			}
		} else {
			record.TokensIn, record.TokensOut = usage.TokensIn, usage.TokensOut
			record.CacheRead, record.CacheWrite = usage.CacheRead, usage.CacheWrite
			record.Reasoning = usage.Reasoning
		}
	}
	retentionErr := completeRunLog(logPath)
	if retentionErr != nil {
		retentionErr = fmt.Errorf("retain completed Run logs: %w", retentionErr)
		if record.Exit == 0 {
			record.Exit = 1
		}
	}
	appendErr := AppendRun(r.Root, record)
	if err := errors.Join(profileErr, invokeErr, cleanupErr, finalizeErr, usageErr, retentionErr, appendErr); err != nil {
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
	runnerPrivateNotesPrefix          = "refs/notes/forest/private/"
)

func (r *Runner) cleanupWorktree(path, runID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	return r.removeWorktree(ctx, path, runID)
}

func (r *Runner) removeWorktree(ctx context.Context, path, runID string) error {
	removeCtx, cancelRemove := context.WithTimeout(ctx, cleanupRemoveExecutionTimeout)
	privateErr := r.cleanupPrivateRefs(removeCtx, runID)
	if privateErr != nil {
		privateErr = fmt.Errorf("delete private notes refs: %w", privateErr)
	}
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
	return errors.Join(privateErr, removeErr, filesystemErr, pruneErr)
}

func (r *Runner) cleanupPrivateRefs(ctx context.Context, runID string) error {
	prefix := runnerPrivateNotesPrefix + runID + "/"
	output, listErr := r.git(ctx, r.Root, "for-each-ref", "--format=%(refname)", prefix)
	if listErr != nil {
		listErr = fmt.Errorf("enumerate: %w", listErr)
	}
	var commands strings.Builder
	var invalidErr error
	for _, ref := range strings.Fields(string(output)) {
		if !strings.HasPrefix(ref, prefix) {
			invalidErr = errors.Join(invalidErr, fmt.Errorf("ref outside run prefix: %s", ref))
			continue
		}
		commands.WriteString("delete ")
		commands.WriteString(ref)
		commands.WriteByte('\n')
	}
	var deleteErr error
	if commands.Len() > 0 {
		_, deleteErr = r.gitInput(ctx, r.Root, strings.NewReader(commands.String()), "update-ref", "--no-deref", "--stdin")
		if deleteErr != nil {
			deleteErr = fmt.Errorf("delete: %w", deleteErr)
		}
	}
	return errors.Join(listErr, invalidErr, deleteErr)
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
	return r.gitInput(ctx, dir, nil, args...)
}

func (r *Runner) gitInput(ctx context.Context, dir string, input io.Reader, args ...string) ([]byte, error) {
	path, err := trustedExecutable(r.Root, r.GitPath)
	if err != nil {
		return nil, err
	}
	command := exec.Command(path, args...)
	command.Dir = dir
	command.Stdin = input
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
	output := &boundedTransportOutput{}
	read := make(chan error, 1)
	go func() {
		_, err := io.Copy(output, reader)
		read <- err
	}()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	var runErr, cleanupErr error
	select {
	case runErr = <-wait:
		cleanupErr = stopResidualProcessGroup(command.Process.Pid, processStopGrace)
		if contextErr := ctx.Err(); contextErr != nil {
			runErr = errors.Join(contextErr, runErr)
		}
	case <-ctx.Done():
		runErr = ctx.Err()
		cleanupErr = stopProcessGroup(command.Process.Pid, wait, processStopGrace)
	}
	var readerCloseErr error
	if cleanupErr != nil {
		readerCloseErr = reader.Close()
	}
	readErr := <-read
	if cleanupErr == nil {
		readerCloseErr = reader.Close()
	}
	return output.data, errors.Join(runErr, cleanupErr, writerCloseErr, readErr, readerCloseErr, output.err())
}

func soleExitCode(err error, code int) bool {
	leaves, matching := 0, 0
	var visit func(error)
	visit = func(err error) {
		switch current := err.(type) {
		case nil:
			return
		case interface{ Unwrap() []error }:
			children := current.Unwrap()
			if len(children) == 0 {
				leaves++
			}
			for _, child := range children {
				visit(child)
			}
		case interface{ Unwrap() error }:
			if child := current.Unwrap(); child != nil {
				visit(child)
			} else {
				leaves++
			}
		default:
			leaves++
			exitErr, ok := current.(*exec.ExitError)
			if ok && exitErr.ProcessState != nil && exitErr.ProcessState.ExitCode() == code {
				matching++
			}
		}
	}
	visit(err)
	return leaves == 1 && matching == 1
}

func (r *Runner) invoke(ctx context.Context, worktree string, declaration Declaration, profileDir string, timeoutSeconds int, logFile io.Writer, record *RunRecord) error {
	if err := ctx.Err(); err != nil {
		record.Exit = contextExit(err)
		return err
	}
	path, err := r.piExecutable()
	if err != nil {
		record.Exit = harnessUnavailableExit
		return err
	}
	// ADR 0018 states this shape. The Runner owns the working directory and the
	// deadline, so it does not restate either to the harness. Project-local
	// harness configuration is trusted: the repository's own skills and
	// extensions are the tools an agent is meant to use, exactly like the
	// AGENTS.md the harness already loads.
	args := []string{
		"-p", "--mode", "json", "--no-session", "--approve",
		"--model", declaration.Model,
		"--system-prompt", declaration.SystemPrompt,
	}
	if len(declaration.Tools) > 0 {
		args = append(args, "--tools", strings.Join(declaration.Tools, ","))
	}
	if declaration.Thinking != "" {
		args = append(args, "--thinking", declaration.Thinking)
	}
	args = append(args, declaration.TaskPrompt)
	command := exec.Command(path, args...)
	command.Dir = worktree
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	name := "Iron Forest " + strings.ToUpper(declaration.Name[:1]) + declaration.Name[1:]
	email := declaration.Name + "@forest.invalid"
	command.Env, err = runEnvironment(r.Root, name, email, record.RunID, profileDir, declaration)
	if err != nil {
		record.Exit = harnessUnavailableExit
		return err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		record.Exit = harnessUnavailableExit
		return fmt.Errorf("open harness output pipe: %w", err)
	}
	command.Stdout = writer
	command.Stderr = writer
	if err := command.Start(); err != nil {
		record.Exit = harnessUnavailableExit
		return errors.Join(fmt.Errorf("start omp: %w", err), writer.Close(), reader.Close())
	}
	writerCloseErr := writer.Close()
	read := make(chan error, 1)
	go func() {
		_, err := io.Copy(logFile, reader)
		read <- err
	}()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	var runErr, cleanupErr error
	timedOut := false
	select {
	case waitErr := <-wait:
		record.Exit, runErr = processResult(ctx, waitErr)
		cleanupErr = stopResidualProcessGroup(command.Process.Pid, processStopGrace)
		if cleanupErr != nil && record.Exit == 0 {
			record.Exit = 1
		}
	case <-ctx.Done():
		cleanupErr = stopProcessGroup(command.Process.Pid, wait, processStopGrace)
		record.Exit = contextExit(ctx.Err())
		if record.Exit == 124 {
			timedOut = true
			runErr = fmt.Errorf("omp timed out after %ds", timeoutSeconds)
		} else {
			runErr = ctx.Err()
		}
	}
	var readerCloseErr error
	if cleanupErr != nil {
		readerCloseErr = reader.Close()
	}
	readErr := <-read
	if cleanupErr == nil {
		readerCloseErr = reader.Close()
	}
	if timedOut {
		_, _ = fmt.Fprintln(logFile, "omp wall-clock timeout")
	}
	err = errors.Join(runErr, cleanupErr, writerCloseErr, readErr, readerCloseErr)
	if err != nil && record.Exit == 0 {
		record.Exit = 1
	}
	return err
}

const (
	processStopGrace       = time.Second
	processGroupProbeLimit = 250 * time.Millisecond
	processGroupProbeStep  = 10 * time.Millisecond
)

func stopProcessGroup(pid int, wait <-chan error, grace time.Duration) error {
	if processGroupExists(pid) {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	timer := time.NewTimer(grace)
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

func stopResidualProcessGroup(pid int, grace time.Duration) error {
	if !processGroupExists(pid) {
		return nil
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	if waitProcessGroupQuiescence(pid, grace) {
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

// piExecutable resolves the agent harness through the trusted PATH, exactly as
// git and gh are resolved. The service unit and the installer put the version
// manager's shim directory on that PATH, so no probing of the operator's home is
// needed and a stubbed PATH is honoured.
func (r *Runner) piExecutable() (string, error) {
	return trustedExecutable(r.Root, r.PiPath)
}

// runnerControlledEnvPrefixes are the environment variables the Kernel owns.
// Anything inherited with one of these prefixes is replaced, never merged.
var runnerControlledEnvPrefixes = []string{"PATH=", "FOREST_RUN_ID=", "GIT_AUTHOR_NAME=", "GIT_AUTHOR_EMAIL=", "GIT_COMMITTER_NAME=", "GIT_COMMITTER_EMAIL="}

// runEvidenceLine is the Run's manifest: the model and its source, the profile
// files the agent saw, and the declared environment's keys. Values never appear,
// because a mint marker or a literal is not the reader's business; the shape is.
// The line is JSON, so the harness output parser reads past it, and typed, so a
// consumer can distinguish it from harness events.
func runEvidenceLine(record RunRecord, declaration Declaration, profileFiles []string) string {
	envKeys := make([]string, 0, len(declaration.Env))
	for key := range declaration.Env {
		envKeys = append(envKeys, key)
	}
	slices.Sort(envKeys)
	line, err := json.Marshal(map[string]any{
		"type":         "forest.run",
		"run_id":       record.RunID,
		"agent":        declaration.Name,
		"model":        declaration.Model,
		"model_source": declaration.ModelSource,
		"profile":      profileFiles,
		"env":          envKeys,
	})
	if err != nil {
		return fmt.Sprintf("{\"type\":\"forest.run\",\"run_id\":%q}", record.RunID)
	}
	return string(line)
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

func parseAgentUsage(path string) (Usage, error) {
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
					return Usage{}, fmt.Errorf("harness usage line %d: %w", lineNumber, usageErr)
				}
				if ok {
					latest = usage
					found = true
				}
				if object, ok := value.(map[string]any); ok && object["type"] == "turn_end" {
					usage, ok, usageErr = findUsage(object["message"])
					if usageErr != nil {
						return Usage{}, fmt.Errorf("harness usage line %d: %w", lineNumber, usageErr)
					}
					if ok {
						total, usageErr = addUsage(total, usage)
						if usageErr != nil {
							return Usage{}, fmt.Errorf("harness usage line %d: %w", lineNumber, usageErr)
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
		return Usage{}, fmt.Errorf("harness output has no usage")
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
