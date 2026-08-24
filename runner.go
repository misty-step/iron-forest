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
	Root       string
	GitPath    string
	PiPath     string
	PrimaryRef string
	Repo       string
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

// piOpenRouterTraceMetadata is the deterministic trace identity sent as a
// free-form samplingParam on every OpenRouter request. It keeps the existing
// session-affinity correlation (the Run ID is still sent as x-session-id) and
// additionally gives Broadcast destinations such as Langfuse a stable trace
// identity that does not depend on provider-side session mapping.
type piOpenRouterTraceMetadata struct {
	TraceID       string `json:"trace_id"`
	TraceName     string `json:"trace_name"`
	Environment   string `json:"environment"`
	Release       string `json:"release"`
	Repo          string `json:"repo"`
	Agent         string `json:"agent"`
	RunID         string `json:"run_id"`
	DefinitionSHA string `json:"definition_sha"`
}

type piOpenRouterSamplingParams struct {
	Trace piOpenRouterTraceMetadata `json:"trace"`
}

type piOpenRouterCompat struct {
	SendSessionAffinityHeaders bool   `json:"sendSessionAffinityHeaders"`
	SessionAffinityFormat      string `json:"sessionAffinityFormat"`
}

type piOpenRouterProvider struct {
	Compat         piOpenRouterCompat                   `json:"compat"`
	ModelOverrides map[string]piOpenRouterModelOverride `json:"modelOverrides,omitempty"`
}

type piOpenRouterModelOverride struct {
	SamplingParams piOpenRouterSamplingParams `json:"samplingParams"`
}

type piOpenRouterModels struct {
	Providers map[string]piOpenRouterProvider `json:"providers"`
}

func piOpenRouterConfig(runID string, declaration Declaration, repo string) ([]byte, error) {
	trace := piOpenRouterTraceMetadata{
		TraceID:       runID,
		TraceName:     "forest/" + declaration.Name,
		Environment:   "production",
		Release:       buildSHA,
		Repo:          repo,
		Agent:         declaration.Name,
		RunID:         runID,
		DefinitionSHA: declaration.DefinitionSHA,
	}
	provider := piOpenRouterProvider{
		Compat: piOpenRouterCompat{
			SendSessionAffinityHeaders: true,
			SessionAffinityFormat:      "openrouter",
		},
	}
	_, modelID, found := strings.Cut(declaration.Model, "/")
	if found && modelID != "" {
		provider.ModelOverrides = map[string]piOpenRouterModelOverride{
			modelID: {SamplingParams: piOpenRouterSamplingParams{Trace: trace}},
		}
	}
	config := piOpenRouterModels{
		Providers: map[string]piOpenRouterProvider{
			"openrouter": provider,
		},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

var (
	errTrustedTransportOutputOverflow = errors.New("trusted transport output exceeded 1 MiB")
	runLogRegistry                    = struct {
		sync.Mutex
		active map[string]struct{}
	}{active: make(map[string]struct{})}
)

// trustedExecutable resolves a tool to a path outside the repository. Symlinks
// are followed to decide trust, because the target is what actually runs, but the
// caller's own path is returned to execute. A version-manager shim dispatches on
// its own name, so running the resolved target would run the manager instead of
// the tool.
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
	return absolute, nil
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
		// Resolve to decide trust, keep the caller's entry to hand to the child.
		// A shim directory reached through a symlink must stay a shim directory,
		// or the agent's own tools break the way the harness did.
		resolved := absolute
		if value, err := filepath.EvalSymlinks(absolute); err == nil {
			resolved = value
		}
		inside, err := pathInside(root, resolved)
		if err != nil {
			return "", err
		}
		if !inside {
			entries = append(entries, absolute)
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

var overriddenChildEnvNames = []string{
	"PATH", "FOREST_RUN_ID", "FOREST_ROOT", "FOREST_PRIMARY_REF", "PI_CODING_AGENT_DIR",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
	"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
	"GIT_CONFIG_KEY_1", "GIT_CONFIG_VALUE_1",
}

func childEnvironment() []string {
	environment := make([]string, 0, len(os.Environ()))
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if !slices.Contains(overriddenChildEnvNames, key) {
			environment = append(environment, value)
		}
	}
	return environment
}

// runEnvironment composes the child's inherited service values, trusted PATH,
// scoped Run Git identity and marker, and fresh writable Pi directory.
func runEnvironment(root, name, email, runID, piDir, primaryRef string) ([]string, error) {
	path, err := trustedPath(root)
	if err != nil {
		return nil, err
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve checkout root: %w", err)
	}
	environment := childEnvironment()
	environment = append(environment,
		"PATH="+path,
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=user.name",
		"GIT_CONFIG_VALUE_0="+name,
		"GIT_CONFIG_KEY_1=user.email",
		"GIT_CONFIG_VALUE_1="+email,
		"FOREST_RUN_ID="+runID,
		"FOREST_ROOT="+absoluteRoot,
		"FOREST_PRIMARY_REF="+primaryRef,
		"PI_CODING_AGENT_DIR="+piDir,
	)
	return environment, nil
}

func configurePiSessionAffinity(piDir, runID string, declaration Declaration, repo string) error {
	provider, modelID, found := strings.Cut(declaration.Model, "/")
	if !found || provider != "openrouter" || modelID == "" {
		return nil
	}
	config, err := piOpenRouterConfig(runID, declaration, repo)
	if err != nil {
		return fmt.Errorf("build Pi session-affinity override: %w", err)
	}
	if err := os.WriteFile(filepath.Join(piDir, "models.json"), config, 0o600); err != nil {
		return fmt.Errorf("write Pi session-affinity override: %w", err)
	}
	return nil
}

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

func (r *Runner) Run(ctx context.Context, declaration Declaration) (RunRecord, error) {
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

	// Publish the live Run record before any preparation work so `forest
	// status` can report age while the Run is still in flight. The record is
	// removed on every return path below.
	livePath := liveRunPath(r.Root, declaration.Name)
	if err := writeLiveRun(livePath, liveRunRecord{RunID: runID, Agent: declaration.Name, StartedAt: record.Started}); err != nil {
		_, _ = fmt.Fprintf(logFile, "publish live run: %v\n", err)
	} else {
		defer func() { _ = os.Remove(livePath) }()
	}

	worktree := forestPath(r.Root, "worktrees", runID)
	primaryRef, worktreeMayExist, prepareErr := r.prepareWorktree(ctx, worktree)
	if prepareErr != nil {
		var cleanupErr error
		if worktreeMayExist {
			cleanupErr = r.cleanupWorktree(worktree, runID)
			if cleanupErr != nil {
				cleanupErr = fmt.Errorf("cleanup worktree: %w", cleanupErr)
				_, _ = fmt.Fprintln(logFile, cleanupErr)
			}
		}
		record.Exit = runContextExit(ctx, 1)
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

	skillErr := validateDeclarationSkillPaths(worktree, declaration.Name, declaration.SkillPaths)
	if skillErr != nil {
		skillErr = fmt.Errorf("validate Run skills: %w", skillErr)
		record.Exit = runContextExit(ctx, 1)
		_, _ = fmt.Fprintln(logFile, skillErr)
	}

	var piDir string
	var piErr error
	if skillErr == nil {
		// Pi state is isolated in a fresh OS temporary directory. Credentials
		// remain in the inherited service environment; no operator Pi files
		// enter the Run.
		piDir, piErr = os.MkdirTemp("", "iron-forest-pi-")
		if piErr == nil {
			piErr = configurePiSessionAffinity(piDir, record.RunID, declaration, r.Repo)
		}
		if piErr != nil {
			piErr = fmt.Errorf("prepare Run Pi directory: %w", piErr)
			record.Exit = runContextExit(ctx, 1)
			_, _ = fmt.Fprintln(logFile, piErr)
		} else {
			_, _ = fmt.Fprintln(logFile, runEvidenceLine(record, declaration))
		}
	}
	harnessRunnable := skillErr == nil && piErr == nil
	var runDeadline time.Time
	if declaration.MaxDuration > 0 {
		runDeadline = started.Add(time.Duration(declaration.MaxDuration) * time.Second)
	}
	var invokeErr error
	harnessStarted := false
	if harnessRunnable {
		invokeErr, harnessStarted = r.invoke(ctx, worktree, declaration, piDir, logFile, primaryRef, &record, runDeadline)
	}
	if hasRunCancellationMarker(r.Root, runID) {
		record.Error = runCancelledError
		record.Exit = runCancelledExit
		invokeErr = errors.Join(invokeErr, errRunCancelled)
	}
	var piCleanupErr error
	if piDir != "" {
		if removeErr := r.cleanupFilesystem(piDir); removeErr != nil {
			piCleanupErr = fmt.Errorf("cleanup Run Pi directory: %w", removeErr)
			_, _ = fmt.Fprintln(logFile, piCleanupErr)
			if record.Exit == 0 {
				record.Exit = 1
			}
		}
	}
	cleanupErr := r.cleanupWorktree(worktree, runID)
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("cleanup worktree: %w", cleanupErr)
		_, _ = fmt.Fprintln(logFile, cleanupErr)
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
	// Usage exists only if the harness started. Demanding it after a refusal to
	// start would report "no usage" as the cause and bury the real one.
	var usageErr error
	if harnessStarted {
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
	if appendErr == nil {
		// The cancellation marker exists only until the Runner has recorded the
		// cancelled outcome. Once the Ledger row exists, a later cancel finds it
		// first, so marker removal is best-effort and never changes the outcome.
		_ = os.Remove(runCancellationMarkerPath(r.Root, runID))
	}
	if err := errors.Join(skillErr, piErr, invokeErr, piCleanupErr, cleanupErr, finalizeErr, usageErr, retentionErr, appendErr); err != nil {
		return record, err
	}
	if record.Exit != 0 {
		if record.Error != "" {
			return record, fmt.Errorf("agent %s %s", declaration.Name, record.Error)
		}
		return record, fmt.Errorf("agent %s exited with %d", declaration.Name, record.Exit)
	}
	return record, nil
}

func newRunID(agent string, now time.Time) string {
	return fmt.Sprintf("%d-%s", now.UnixNano(), agentSlug(agent))
}

func (r *Runner) primaryRef(ctx context.Context) (string, error) {
	if r.PrimaryRef != "" {
		return r.PrimaryRef, nil
	}
	cfg, err := loadConfig(configPath(r.Root))
	if err != nil {
		return "", err
	}
	ref, _, err := resolvePrimary(ctx, r.Root, cfg)
	if err != nil {
		return "", fmt.Errorf("resolve primary ref: %w", err)
	}
	return ref, nil
}

func (r *Runner) prepareWorktree(ctx context.Context, path string) (string, bool, error) {
	primary, err := r.primaryRef(ctx)
	if err != nil {
		return "", false, err
	}
	branch := strings.TrimPrefix(primary, primaryRefPrefix)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", false, err
	}
	if _, err := r.git(ctx, r.Root, "fetch", "origin", branch); err != nil {
		return "", false, fmt.Errorf("fetch origin/%s: %w", branch, err)
	}
	if _, err := r.git(ctx, r.Root, "worktree", "add", "--detach", path, "origin/"+branch); err != nil {
		return "", true, fmt.Errorf("add worktree: %w", err)
	}
	return primary, true, nil
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

func (r *Runner) cleanupFilesystem(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupFilesystemExecutionTimeout)
	defer cancel()
	return r.removeFilesystem(ctx, path)
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

const agentOutcomePatternLimit = 32

var (
	agentEndPattern          = []byte(`"type":"agent_end"`)
	agentAssistantPattern    = []byte(`"role":"assistant"`)
	agentErrorPattern        = []byte(`"stopReason":"error"`)
	agentErrorMessagePattern = []byte(`"errorMessage":"`)
)

type streamPattern struct {
	pattern []byte
	prefix  [agentOutcomePatternLimit]int
	matched int
}

func (m *streamPattern) init(pattern []byte) {
	m.pattern = pattern
	for index := 1; index < len(pattern); index++ {
		fallback := m.prefix[index-1]
		for fallback > 0 && pattern[index] != pattern[fallback] {
			fallback = m.prefix[fallback-1]
		}
		if pattern[index] == pattern[fallback] {
			fallback++
		}
		m.prefix[index] = fallback
	}
}

func (m *streamPattern) advance(value byte) bool {
	for m.matched > 0 && value != m.pattern[m.matched] {
		m.matched = m.prefix[m.matched-1]
	}
	if value == m.pattern[m.matched] {
		m.matched++
	}
	if m.matched != len(m.pattern) {
		return false
	}
	m.matched = m.prefix[m.matched-1]
	return true
}

// agentOutcomeTracker observes the unbounded Pi stream while the Run log keeps
// only its bounded head and tail. Structural JSON strings remain unescaped in
// events and escaped inside message content, so the tracker can identify the
// latest assistant outcome without retaining an arbitrarily large agent_end.
type agentOutcomeTracker struct {
	agentEnd             streamPattern
	agentAssistant       streamPattern
	agentError           streamPattern
	agentErrorMessage    streamPattern
	lineActive           bool
	sawAgentEnd          bool
	sawAssistant         bool
	awaitingErrorMessage bool
	assistantError       bool
	failed               bool
}

func newAgentOutcomeTracker() *agentOutcomeTracker {
	tracker := &agentOutcomeTracker{}
	tracker.agentEnd.init(agentEndPattern)
	tracker.agentAssistant.init(agentAssistantPattern)
	tracker.agentError.init(agentErrorPattern)
	tracker.agentErrorMessage.init(agentErrorMessagePattern)
	return tracker
}

func (t *agentOutcomeTracker) Write(data []byte) (int, error) {
	for _, value := range data {
		if value == '\n' {
			t.finishLine()
			continue
		}
		if t.awaitingErrorMessage {
			if value != '"' {
				t.assistantError = true
			}
			t.awaitingErrorMessage = false
		}
		t.lineActive = true
		if t.agentEnd.advance(value) {
			t.sawAgentEnd = true
		}
		if t.agentAssistant.advance(value) {
			t.sawAssistant = true
			t.awaitingErrorMessage = false
			t.assistantError = false
		}
		if t.agentError.advance(value) && t.sawAssistant {
			t.assistantError = true
		}
		if t.agentErrorMessage.advance(value) && t.sawAssistant {
			t.awaitingErrorMessage = true
		}
	}
	return len(data), nil
}

func (t *agentOutcomeTracker) finishLine() {
	if t.sawAgentEnd && t.sawAssistant {
		t.failed = t.assistantError
	}
	t.agentEnd.matched = 0
	t.agentAssistant.matched = 0
	t.agentError.matched = 0
	t.agentErrorMessage.matched = 0
	t.lineActive = false
	t.sawAgentEnd = false
	t.sawAssistant = false
	t.awaitingErrorMessage = false
	t.assistantError = false
}

func (t *agentOutcomeTracker) Err() error {
	if t.lineActive {
		t.finishLine()
	}
	if t.failed {
		return errors.New("pi agent ended with error")
	}
	return nil
}

func (r *Runner) invoke(ctx context.Context, worktree string, declaration Declaration, piDir string, logFile io.Writer, primaryRef string, record *RunRecord, deadline time.Time) (err error, started bool) {
	if err := ctx.Err(); err != nil {
		record.Exit = contextExit(err)
		return err, false
	}
	path, err := r.piExecutable()
	if err != nil {
		record.Exit = harnessUnavailableExit
		return err, false
	}
	// Pi receives a complete explicit resource contract. Global extensions,
	// skills, prompt templates, and themes are disabled; each declared skill is
	// resolved from the Run worktree.
	args := []string{
		"-p", "--mode", "json", "--no-session", "--session-id", record.RunID, "--approve",
		"--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes",
		"--model", declaration.Model,
		"--system-prompt", declaration.SystemPrompt,
	}
	for _, skill := range declaration.SkillPaths {
		args = append(args, "--skill", skill)
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
	command.Env, err = runEnvironment(r.Root, name, email, record.RunID, piDir, primaryRef)
	if err != nil {
		record.Exit = harnessUnavailableExit
		return err, false
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		record.Exit = harnessUnavailableExit
		return fmt.Errorf("open harness output pipe: %w", err), false
	}
	command.Stdout = writer
	command.Stderr = writer
	if err := r.verifyDeclarationDigest(declaration); err != nil {
		record.Exit = 1
		return errors.Join(err, writer.Close(), reader.Close()), false
	}
	record.DefinitionSHA = declaration.DefinitionSHA
	if err := command.Start(); err != nil {
		record.Exit = harnessUnavailableExit
		return errors.Join(fmt.Errorf("start pi: %w", err), writer.Close(), reader.Close()), false
	}
	writerCloseErr := writer.Close()
	outcome := newAgentOutcomeTracker()
	read := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.MultiWriter(logFile, outcome), reader)
		read <- err
	}()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	var watchdogTimer *time.Timer
	var watchdog <-chan time.Time
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		watchdogTimer = time.NewTimer(remaining)
		watchdog = watchdogTimer.C
		defer watchdogTimer.Stop()
	}
	var runErr, cleanupErr error
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
		runErr = ctx.Err()
	case <-watchdog:
		if markerErr := writeRunCancellationMarker(r.Root, record.RunID); markerErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("write run cancellation marker: %w", markerErr))
		}
		cleanupErr = stopProcessGroup(command.Process.Pid, wait, processStopGrace)
		record.Exit = runCancelledExit
		record.Error = runCancelledError
	}
	var readerCloseErr error
	if cleanupErr != nil {
		readerCloseErr = reader.Close()
	}
	readErr := <-read
	if cleanupErr == nil {
		readerCloseErr = reader.Close()
	}

	err = errors.Join(runErr, cleanupErr, writerCloseErr, readErr, readerCloseErr, outcome.Err())
	if err != nil && record.Exit == 0 {
		record.Exit = 1
	}
	return err, true
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

func runContextExit(ctx context.Context, fallback int) int {
	if err := ctx.Err(); err != nil {
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

// verifyDeclarationDigest recomputes the digest over the ordered declaration
// pair (agent.md then task.md) from the repository root and compares it with the
// digest loadDeclaration recorded. The run is refused before Pi starts when any
// declared file changed after load, because the executed prompt, model, and tool
// set are otherwise not provably the declared ones (see #144). A declaration
// without a recorded digest (for example a directly constructed predecessor)
// has nothing to compare and dispatches normally.
func (r *Runner) verifyDeclarationDigest(declaration Declaration) error {
	if declaration.DefinitionSHA == "" {
		return nil
	}
	dir := declarationDir(r.Root, declaration.Name)
	agentData, err := os.ReadFile(filepath.Join(dir, "agent.md"))
	if err != nil {
		return fmt.Errorf("re-read %s agent.md: %w", declaration.Name, err)
	}
	taskData, err := os.ReadFile(filepath.Join(dir, "task.md"))
	if err != nil {
		return fmt.Errorf("re-read %s task.md: %w", declaration.Name, err)
	}
	if got := declarationPairDigest(agentData, taskData); got != declaration.DefinitionSHA {
		return fmt.Errorf("agent %s bundle changed since load: digest %s != recorded %s", declaration.Name, got, declaration.DefinitionSHA)
	}
	return nil
}

// runEvidenceLine describes the explicit resources and non-secret settings Pi
// received. The line is typed JSON so consumers distinguish it from Pi events.
func runEvidenceLine(record RunRecord, declaration Declaration) string {
	skills := declaration.SkillPaths
	if skills == nil {
		skills = []string{}
	}
	line, _ := json.Marshal(map[string]any{
		"type":         "forest.run",
		"run_id":       record.RunID,
		"agent":        declaration.Name,
		"model":        declaration.Model,
		"model_source": declaration.ModelSource,
		"skills":       skills,
	})
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
