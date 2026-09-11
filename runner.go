package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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
	Scope      Scope
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
	"FOREST_SCOPE_LABEL", "FOREST_SCOPE_BRANCH_PREFIX", "FOREST_SCOPE_SUBJECTS", "FOREST_SCOPE_GITHUB_ONLY",
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

// scopeEnvironment exports the effective selection scope as Run environment
// values. The zero Scope exports the default label and empty remaining modes,
// which matches the Poller's default GitHub label and unrestricted selection.
// A configured label scope is exported as an explicit GitHub-only signal so
// the Builder never has to infer it from the label value (a label scope is
// GitHub-only even when the label equals the default forest:ready).
func scopeEnvironment(scope Scope) []string {
	label := defaultReadyLabel
	githubOnly := "0"
	if scope.Label != "" {
		label = scope.Label
		githubOnly = "1"
	}
	return []string{
		"FOREST_SCOPE_LABEL=" + label,
		"FOREST_SCOPE_BRANCH_PREFIX=" + scope.BranchPrefix,
		"FOREST_SCOPE_SUBJECTS=" + strings.Join(scope.Subjects, ","),
		"FOREST_SCOPE_GITHUB_ONLY=" + githubOnly,
	}
}

// openRouterRoleKeyName names the instance environment variable that holds the
// role-scoped OpenRouter completion key for one agent role. The role name is
// the declaration name, so builder selects OPENROUTER_API_KEY_BUILDER.
func openRouterRoleKeyName(role string) string {
	return "OPENROUTER_API_KEY_" + strings.ToUpper(role)
}

// openRouterCompletionKey selects the OpenRouter completion key a Run receives:
// the role-scoped key when it is set, otherwise the instance-wide
// OPENROUTER_API_KEY. Selecting only by environment name keeps the role key out
// of Kernel state and preserves the existing single-key deployment as the
// fallback for any role without a dedicated key.
func openRouterCompletionKey(role string) string {
	if role != "" {
		if value := strings.TrimSpace(os.Getenv(openRouterRoleKeyName(role))); value != "" {
			return value
		}
	}
	return strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
}

// withOpenRouterCompletionKey replaces every inherited OPENROUTER_API_KEY and
// role-scoped OPENROUTER_API_KEY_* entry with the single key selected for this
// Run role, so a Run always uses exactly one completion key, does not leak
// sibling role keys, and never hands an ambiguous duplicate to exec.
func withOpenRouterCompletionKey(environment []string, role string) []string {
	selected := openRouterCompletionKey(role)
	filtered := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if key != "OPENROUTER_API_KEY" && !strings.HasPrefix(key, "OPENROUTER_API_KEY_") {
			filtered = append(filtered, entry)
		}
	}
	if selected != "" {
		filtered = append(filtered, "OPENROUTER_API_KEY="+selected)
	}
	return filtered
}

// runEnvironment composes the child's inherited service values, trusted PATH,
// scoped Run Git identity and marker, fresh writable Pi directory, and the
// role-scoped OpenRouter completion key.
func runEnvironment(root, name, email, runID, piDir, primaryRef, role string, scope Scope) ([]string, error) {
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
	environment = append(environment, scopeEnvironment(scope)...)
	return withOpenRouterCompletionKey(environment, role), nil
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

func (r *Runner) Run(ctx context.Context, declaration Declaration) (record RunRecord, runErr error) {
	started := time.Now().UTC()
	runID := declaration.RunID
	if runID == "" {
		runID = newRunID(declaration.Name, started)
	}
	record = RunRecord{RunID: runID, Agent: declaration.Name, Started: started.Format(time.RFC3339Nano),
		DefinitionSHA: declaration.DefinitionSHA, ExtensionSHA: declaration.ExtensionSHA}
	attachRunRequest(&record, declaration.Request)
	logPath := runLogPath(r.Root, runID)
	livePath := liveRunPath(r.Root, declaration.Name)
	worktree := forestPath(r.Root, "worktrees", runID)
	var logFile *boundedRunLog
	var piDir string
	var worktreeMayExist, harnessStarted, evidenceWritten bool
	defer func() {
		// Classify setup at its return boundary, before cleanup can outlive the
		// caller's deadline. Harness outcomes were already recorded at source.
		if record.Outcome == "" {
			record.Outcome = runOutcomeSetupFailed
			if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runErr, context.Canceled) {
				recordContextFailure(&record, runErr)
			}
		}
		if applyRunCancellation(r.Root, &record) && !errors.Is(runErr, errRunCancelled) {
			runErr = errors.Join(runErr, errRunCancelled)
		}
		// Retain known execution facts before deferred cleanup; recovery must
		// not turn an observed timeout into an operator cancellation.
		if err := writeLiveRun(livePath, liveRecord(record)); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("retain Run execution: %w", err))
		}
		var finalizationErr error
		if piDir != "" {
			// Read the Run's provider receipt before its Pi directory is
			// disposable; a missing or malformed receipt leaves the charge
			// unknown and never fails an otherwise successful Run.
			record.ProviderCost = readProviderCost(piDir)
			finalizationErr = errors.Join(finalizationErr, r.cleanupFilesystem(piDir))
		}
		if logFile != nil {
			if !evidenceWritten {
				_, _ = fmt.Fprintln(logFile, runEvidenceLine(record, declaration))
			}
			if runErr != nil {
				_, _ = fmt.Fprintln(logFile, runErr)
			}
			finalizationErr = errors.Join(finalizationErr, logFile.Finalize())
			if harnessStarted {
				usage, err := parseAgentUsage(logPath)
				if err != nil {
					finalizationErr = errors.Join(finalizationErr, fmt.Errorf("parse harness usage: %w", err))
				} else {
					record.TokensIn, record.TokensOut = usage.TokensIn, usage.TokensOut
					record.CacheRead, record.CacheWrite = usage.CacheRead, usage.CacheWrite
					record.Reasoning = usage.Reasoning
				}
			}
			finalizationErr = errors.Join(finalizationErr, completeRunLog(logPath))
		}
		if worktreeMayExist {
			// Discover log/usage failures before deciding that source is disposable.
			if runErr == nil && finalizationErr == nil && (record.Outcome == runOutcomeCompleted || record.Outcome == runOutcomeNoWork) {
				finalizationErr = errors.Join(finalizationErr, r.cleanupWorktree(worktree, runID))
			}
			if runErr != nil || finalizationErr != nil || (record.Outcome != runOutcomeCompleted && record.Outcome != runOutcomeNoWork) {
				ctx, cancel := context.WithTimeout(context.Background(), reservedCleanupTimeout)
				record.Recovery = r.retainedWorktree(ctx, runID)
				cancel()
			}
		}
		// Preserve the first execution cause. Cleanup or usage failures make an
		// otherwise successful attempt an internal error, never alter raw Pi exit.
		runErr = errors.Join(runErr, finalizationErr)
		if runErr != nil && (record.Outcome == runOutcomeCompleted || record.Outcome == runOutcomeNoWork) {
			record.Outcome = runOutcomeInternalError
		}
		if applyRunCancellation(r.Root, &record) && !errors.Is(runErr, errRunCancelled) {
			runErr = errors.Join(runErr, errRunCancelled)
		}
		record.Duration = time.Since(started).Seconds()
		if runErr != nil {
			record.NoWork = false
			if record.Exit == 0 {
				record.Exit = 1
			}
			record.Error = runErr.Error()
		}
		if record.Exit != 0 && runErr == nil && !record.NoWork {
			runErr = fmt.Errorf("agent %s exited with %d", declaration.Name, record.Exit)
			if record.Error == "" {
				record.Error = runErr.Error()
			}
		}
		finalLive := liveRecord(record)
		finalLive.Finalized = true
		if err := writeLiveRun(livePath, finalLive); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("retain finalized Run: %w", err))
			if record.Outcome == runOutcomeCompleted || record.Outcome == runOutcomeNoWork {
				record.Outcome = runOutcomeInternalError
			}
			if record.Exit == 0 {
				record.Exit = 1
			}
			record.NoWork = false
			if record.Error == "" {
				record.Error = runErr.Error()
			}
		}
		if err := AppendRun(r.Root, record); err != nil {
			// Preserve the durable owner record for recovery, never silently lose
			// request attribution when the final Ledger publication fails.
			runErr = errors.Join(runErr, fmt.Errorf("append Run: %w", err))
			if record.Outcome == runOutcomeCompleted || record.Outcome == runOutcomeNoWork {
				record.Outcome = runOutcomeInternalError
			}
			if record.Exit == 0 {
				record.Exit = 1
			}
			record.NoWork = false
			if record.Error == "" {
				record.Error = runErr.Error()
			}
			failedLive := liveRecord(record)
			failedLive.Finalized = true
			runErr = errors.Join(runErr, writeLiveRun(livePath, failedLive))
		} else {
			_ = os.Remove(livePath)
			_ = os.Remove(runCancellationMarkerPath(r.Root, runID))
		}
	}()
	if err := writeLiveRun(livePath, liveRecord(record)); err != nil {
		return record, fmt.Errorf("publish live Run: %w", err)
	}
	var err error
	logFile, err = openBoundedRunLog(logPath)
	if err != nil {
		return record, err
	}
	if err := r.verifyDeclarationDigest(declaration); err != nil {
		return record, err
	}
	request, err := r.requestForRun(ctx, declaration, record, logFile)
	if errors.Is(err, errRequestNoWork) {
		record.NoWork, record.Exit, record.Outcome = true, exitNoWork, runOutcomeNoWork
		return record, nil
	}
	if err != nil {
		return record, err
	}
	attachRunRequest(&record, request)
	declaration.TaskPrompt = requestPrompt(declaration.TaskPrompt, request)
	if err := writeLiveRun(livePath, liveRecord(record)); err != nil {
		return record, fmt.Errorf("persist Run request association: %w", err)
	}
	_, _ = fmt.Fprintln(logFile, runEvidenceLine(record, declaration))
	evidenceWritten = true
	if request != nil {
		data, err := json.Marshal(request)
		if err != nil {
			return record, err
		}
		path := forestPath(r.Root, "runs", runID+".request.json")
		if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
			return record, fmt.Errorf("retain Run request: %w", err)
		}
	}
	primaryRef, mayExist, err := r.prepareWorktree(ctx, worktree)
	worktreeMayExist = mayExist
	if err != nil {
		return record, fmt.Errorf("prepare worktree: %w", err)
	}
	if err := validateDeclarationSkillPaths(worktree, declaration.Name, declaration.SkillPaths); err != nil {
		return record, fmt.Errorf("validate Run skills: %w", err)
	}
	if err := verifyDeclarationExtensions(worktree, declaration); err != nil {
		return record, fmt.Errorf("validate Run extensions: %w", err)
	}
	// All writable Pi state belongs to this instance, never the operator's home.
	piDir, err = os.MkdirTemp(forestPath(r.Root), "pi-"+runID+"-")
	if err != nil {
		return record, fmt.Errorf("prepare Run Pi directory: %w", err)
	}
	if err := configurePiSessionAffinity(piDir, record.RunID, declaration, r.Repo); err != nil {
		return record, err
	}
	var deadline time.Time
	if declaration.MaxDuration > 0 {
		deadline = started.Add(time.Duration(declaration.MaxDuration) * time.Second)
	}
	runErr, harnessStarted = r.invoke(ctx, worktree, declaration, piDir, logFile, primaryRef, &record, deadline)
	if harnessStarted {
		if applyRunCancellation(r.Root, &record) {
			runErr = errors.Join(runErr, errRunCancelled)
		}
		record.Duration = time.Since(started).Seconds()
		if runErr != nil && record.Error == "" {
			record.Error = runErr.Error()
		}
		if err := writeLiveRun(livePath, liveRecord(record)); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("retain Run execution: %w", err))
			if record.Outcome == runOutcomeCompleted {
				record.Outcome, record.Exit = runOutcomeInternalError, 1
			}
			if record.Error == "" {
				record.Error = runErr.Error()
			}
		}
		// A cancelled model may already have created its required external
		// effect. Give the read observer its own existing command bound, not
		// the model's expired context. This never retries the model.
		record.Completion = r.completionForRun(context.WithoutCancel(ctx), declaration, record, request, logFile)
	}
	return record, runErr
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", false, err
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
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
		_, cleanupErr = stopProcessGroup(command.Process.Pid, wait, processStopGrace)
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

const (
	agentOutcomePatternLimit = 32
	providerBudgetExhausted  = "provider budget exhausted"
)

var (
	agentEndPattern          = []byte(`"type":"agent_end"`)
	agentAssistantPattern    = []byte(`"role":"assistant"`)
	agentErrorPattern        = []byte(`"stopReason":"error"`)
	agentErrorMessagePattern = []byte(`"errorMessage":"`)
	agentBudget402Pattern    = []byte(`"errorMessage":"402`)
	agentBudget403Pattern    = []byte(`"errorMessage":"403`)
)

func isProviderBudgetError(message string) bool {
	return message == providerBudgetExhausted
}

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
	agentBudget402       streamPattern
	agentBudget403       streamPattern
	lineActive           bool
	sawAgentEnd          bool
	sawAssistant         bool
	awaitingErrorMessage bool
	assistantError       bool
	lineBudget           bool
	failed               bool
	budgetFailed         bool
}

func newAgentOutcomeTracker() *agentOutcomeTracker {
	tracker := &agentOutcomeTracker{}
	tracker.agentEnd.init(agentEndPattern)
	tracker.agentAssistant.init(agentAssistantPattern)
	tracker.agentError.init(agentErrorPattern)
	tracker.agentErrorMessage.init(agentErrorMessagePattern)
	tracker.agentBudget402.init(agentBudget402Pattern)
	tracker.agentBudget403.init(agentBudget403Pattern)
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
			t.lineBudget = false
		}
		if t.agentError.advance(value) && t.sawAssistant {
			t.assistantError = true
		}
		if t.agentErrorMessage.advance(value) && t.sawAssistant {
			t.awaitingErrorMessage = true
		}
		if t.sawAssistant && (t.agentBudget402.advance(value) || t.agentBudget403.advance(value)) {
			t.lineBudget = true
			t.assistantError = true
		}
	}
	return len(data), nil
}

func (t *agentOutcomeTracker) finishLine() {
	if t.sawAgentEnd && t.sawAssistant {
		t.failed = t.assistantError
		t.budgetFailed = t.lineBudget
	}
	t.agentEnd.matched = 0
	t.agentAssistant.matched = 0
	t.agentError.matched = 0
	t.agentErrorMessage.matched = 0
	t.agentBudget402.matched = 0
	t.agentBudget403.matched = 0
	t.lineActive = false
	t.sawAgentEnd = false
	t.sawAssistant = false
	t.awaitingErrorMessage = false
	t.assistantError = false
	t.lineBudget = false
}

func (t *agentOutcomeTracker) Err() error {
	if t.lineActive {
		t.finishLine()
	}
	if t.budgetFailed {
		return errors.New(providerBudgetExhausted)
	}
	if t.failed {
		return errors.New("pi agent ended with error")
	}
	return nil
}

func (r *Runner) invoke(ctx context.Context, worktree string, declaration Declaration, piDir string, logFile io.Writer, primaryRef string, record *RunRecord, deadline time.Time) (err error, started bool) {
	if err := ctx.Err(); err != nil {
		recordContextFailure(record, err)
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
	for _, extension := range declaration.ExtensionPaths {
		args = append(args, "--extension", extension)
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
	command.Env, err = runEnvironment(r.Root, name, email, record.RunID, piDir, primaryRef, declaration.Name, r.Scope)
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
	if err := verifyDeclarationExtensions(worktree, declaration); err != nil {
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
	var reaped bool
	select {
	case waitErr := <-wait:
		reaped = true
		record.Exit, runErr = processResult(ctx, waitErr)
		record.Outcome = runOutcomeCompleted
		if record.Exit != 0 {
			record.Outcome = runOutcomeExecutionFailed
		}
		if applyRunCancellation(r.Root, record) {
			runErr = errRunCancelled
		} else if runErr != nil {
			recordContextFailure(record, runErr)
		}
		cleanupErr = stopResidualProcessGroup(command.Process.Pid, processStopGrace)
	case <-ctx.Done():
		if applyRunCancellation(r.Root, record) {
			runErr = errRunCancelled
		} else {
			runErr = ctx.Err()
			recordContextFailure(record, runErr)
		}
		if err := writeLiveRun(liveRunPath(r.Root, record.Agent), liveRecord(*record)); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("retain Run stop cause: %w", err))
		}
		reaped, cleanupErr = stopProcessGroup(command.Process.Pid, wait, processStopGrace)
	case <-watchdog:
		if applyRunCancellation(r.Root, record) {
			runErr = errRunCancelled
		} else {
			record.Outcome, record.Exit, record.Error = runOutcomeTimedOut, runTimedOutExit, runTimedOutError
			runErr = errRunTimedOut
		}
		if err := writeLiveRun(liveRunPath(r.Root, record.Agent), liveRecord(*record)); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("retain Run stop cause: %w", err))
		}
		reaped, cleanupErr = stopProcessGroup(command.Process.Pid, wait, processStopGrace)
	}
	// Only a consumed Wait synchronizes access to ProcessState. If the bounded
	// stop could not reap Pi, its raw exit remains honestly unobserved.
	if reaped && command.ProcessState != nil {
		exit := command.ProcessState.ExitCode()
		record.ProcessExit = &exit
	}
	var readerCloseErr error
	if cleanupErr != nil {
		readerCloseErr = reader.Close()
	}
	readErr := <-read
	if cleanupErr == nil {
		readerCloseErr = reader.Close()
	}

	outcomeErr := outcome.Err()
	if outcomeErr != nil && (record.Outcome == runOutcomeCompleted || record.Outcome == runOutcomeExecutionFailed) {
		record.Outcome = runOutcomeProviderFailed
		if outcome.budgetFailed {
			record.Error = providerBudgetExhausted
		}
	}
	err = errors.Join(runErr, cleanupErr, writerCloseErr, readErr, readerCloseErr, outcomeErr)
	if err != nil && record.Outcome == runOutcomeCompleted {
		record.Outcome = runOutcomeInternalError
	}
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

func stopProcessGroup(pid int, wait <-chan error, grace time.Duration) (reaped bool, err error) {
	if processGroupExists(pid) {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-wait:
		reaped = true
	case <-timer.C:
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		waitTimer := time.NewTimer(processGroupProbeLimit)
		defer waitTimer.Stop()
		select {
		case <-wait:
			reaped = true
		case <-waitTimer.C:
		}
	}
	if processGroupExists(pid) {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
	if !waitProcessGroupQuiescence(pid, processGroupProbeLimit) {
		return reaped, fmt.Errorf("process group %d did not quiesce", pid)
	}
	return reaped, nil
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

func recordContextFailure(record *RunRecord, err error) {
	record.Exit = contextExit(err)
	record.Outcome = runOutcomeInterrupted
	if errors.Is(err, context.DeadlineExceeded) {
		record.Outcome = runOutcomeTimedOut
	}
	record.Error = err.Error()
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
	evidence := map[string]any{
		"type": "forest.run", "run_id": record.RunID, "agent": declaration.Name,
		"model": declaration.Model, "model_source": declaration.ModelSource,
		"skills": skills, "extensions": declaration.ExtensionPaths,
		"definition_sha": declaration.DefinitionSHA, "extension_sha": declaration.ExtensionSHA,
	}
	if record.RequestID != "" {
		evidence["request_id"] = record.RequestID
	}
	if record.Work != nil {
		evidence["work"] = record.Work
	}
	if record.NoWork {
		evidence["no_work"] = true
	}
	line, _ := json.Marshal(evidence)
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

// providerCostFile is the Run-local provider receipt name. The Run's declared
// model extension writes it at the provider transport seam; Pi itself drops
// OpenRouter's charged amount when it recomputes usage.cost from catalog rates.
const providerCostFile = "provider-cost.json"

// readProviderCost reads the optional provider receipt from the Run's agent
// directory. Only the fixed contract is accepted: the configured provider's
// own charged amount as a finite, non-negative number with explicit
// completeness. Missing, unreadable, malformed, or non-finite evidence leaves
// the Run's charge unknown; optional accounting never fails a Run.
func readProviderCost(dir string) *ProviderCost {
	data, err := os.ReadFile(filepath.Join(dir, providerCostFile))
	if err != nil {
		return nil
	}
	var receipt struct {
		Provider string   `json:"provider"`
		CostUSD  *float64 `json:"cost_usd"`
		Complete *bool    `json:"complete"`
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil
	}
	if receipt.Provider != "openrouter" || receipt.CostUSD == nil || receipt.Complete == nil {
		return nil
	}
	amount := *receipt.CostUSD
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount < 0 {
		return nil
	}
	return &ProviderCost{Provider: receipt.Provider, CostUSD: amount, Complete: *receipt.Complete}
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
