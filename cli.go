package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Stable process exit codes (documented, agent-parseable).
const (
	exitOK         = 0
	exitNoWork     = 1
	exitError      = 2
	exitNotFound   = 4
	exitConflict   = 5
	exitInvalidArg = 6
)

// cliFlags carries global and per-command flags that every object command
// understands. Flags may appear before or after the positional command.
type cliFlags struct {
	root   string
	json   bool
	limit  int
	after  string
	follow bool
	rescan bool
}

func parseCLIFlags(args []string) (positional []string, flags cliFlags, err error) {
	flags.limit = -1
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--json":
			flags.json = true
		case arg == "--follow":
			flags.follow = true
		case arg == "--rescan":
			flags.rescan = true
		case arg == "--root" || arg == "-C":
			if index+1 >= len(args) {
				return nil, flags, errors.New("--root requires a directory")
			}
			index++
			flags.root = args[index]
		case strings.HasPrefix(arg, "--root="):
			flags.root = strings.TrimPrefix(arg, "--root=")
		case arg == "--limit":
			if index+1 >= len(args) {
				return nil, flags, errors.New("--limit requires a number")
			}
			index++
			limit, parseErr := requirePositiveLimit(args[index])
			if parseErr != nil {
				return nil, flags, parseErr
			}
			flags.limit = limit
		case strings.HasPrefix(arg, "--limit="):
			limit, parseErr := requirePositiveLimit(strings.TrimPrefix(arg, "--limit="))
			if parseErr != nil {
				return nil, flags, parseErr
			}
			flags.limit = limit
		case arg == "--after":
			if index+1 >= len(args) {
				return nil, flags, errors.New("--after requires a run identity")
			}
			index++
			flags.after = args[index]
		case strings.HasPrefix(arg, "--after="):
			flags.after = strings.TrimPrefix(arg, "--after=")
		case strings.HasPrefix(arg, "-"):
			return nil, flags, fmt.Errorf("unknown flag %q", arg)
		default:
			positional = append(positional, arg)
		}
	}
	if flags.root == "" {
		flags.root, err = os.Getwd()
		if err != nil {
			return nil, flags, fmt.Errorf("resolve working directory: %w", err)
		}
	}
	return positional, flags, nil
}

func requirePositiveLimit(value string) (int, error) {
	var limit int
	if _, err := fmt.Sscanf(value, "%d", &limit); err != nil || limit <= 0 {
		return 0, fmt.Errorf("--limit must be a positive integer, got %q", value)
	}
	return limit, nil
}

// cliOutcome is the normalized result of one object command.
type cliOutcome struct {
	Exit    int
	JSON    any
	Human   string
	ErrText string
}

// runObjectCommand executes one object command under the shared flag surface
// and renders it either as human text or as a forest.cli.v1 envelope.
func runObjectCommand(original []string) int {
	positional, flags, err := parseCLIFlags(original)
	if err != nil {
		return cliFail(exitInvalidArg, err.Error())
	}
	if len(positional) == 0 {
		printUsage()
		return exitInvalidArg
	}
	object := positional[0]
	rest := positional[1:]
	var outcome cliOutcome
	switch object {
	case "config":
		outcome = runConfig(rest, flags)
	case "declaration":
		outcome = runDeclaration(rest, flags)
	case "trigger":
		outcome = runTrigger(rest, flags)
	case "run":
		outcome = runRun(rest, flags)
	case "audit":
		outcome = runAudit(rest, flags)
	default:
		return cliFail(exitInvalidArg, fmt.Sprintf("unknown command %q", object))
	}
	if flags.json {
		emitEnvelope(os.Stdout, strings.Join(positional, " "), outcome.Exit, outcome.JSON, outcome.ErrText)
	} else if outcome.ErrText != "" {
		return cliFail(outcome.Exit, outcome.ErrText)
	} else if outcome.Human != "" {
		fmt.Println(outcome.Human)
	}
	return outcome.Exit
}

func cliFail(exit int, errText string) int {
	fmt.Fprintln(os.Stderr, errText)
	return exit
}

func emitEnvelope(w io.Writer, command string, exit int, data any, errText string) {
	envelope := map[string]any{
		"schema":  "forest.cli.v1",
		"command": command,
		"exit":    exit,
		"data":    data,
		"error":   nilValue(errText),
	}
	_ = json.NewEncoder(w).Encode(envelope)
}

func nilValue(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func runConfig(rest []string, flags cliFlags) cliOutcome {
	if len(rest) != 1 || rest[0] != "show" {
		return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest config show [--json]"}
	}
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return cliOutcome{Exit: exitError, ErrText: err.Error()}
	}
	agents := make(map[string]AgentConfig, len(cfg.Agents))
	for name, agent := range cfg.Agents {
		agents[name] = agent
	}
	data := map[string]any{
		"repo":   cfg.Repo,
		"agents": agents,
		"checks": cfg.Checks,
	}
	human := fmt.Sprintf("repo: %s", cfg.Repo)
	for _, name := range agentNames(cfg) {
		agent := cfg.Agents[name]
		human += fmt.Sprintf("\nagent %s: poll=%q interval=%ds timeout=%ds", name, agent.Poll, agent.Interval, agent.Timeout)
	}
	for _, check := range cfg.Checks {
		human += fmt.Sprintf("\ncheck %s: run=%q", check.Name, check.Run)
	}
	return cliOutcome{Exit: exitOK, JSON: data, Human: human}
}

func runDeclaration(rest []string, flags cliFlags) cliOutcome {
	if len(rest) == 0 {
		return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest declaration list|show <name> [--json]"}
	}
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return cliOutcome{Exit: exitError, ErrText: err.Error()}
	}
	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest declaration list [--json]"}
		}
		declarations := make([]Declaration, 0, len(cfg.Agents))
		for _, name := range agentNames(cfg) {
			declaration, loadErr := loadDeclaration(flags.root, name)
			if loadErr != nil {
				return cliOutcome{Exit: exitError, ErrText: loadErr.Error()}
			}
			declarations = append(declarations, declaration)
		}
		names := make([]string, 0, len(declarations))
		for _, declaration := range declarations {
			names = append(names, declaration.Name)
		}
		return cliOutcome{
			Exit:  exitOK,
			JSON:  map[string]any{"declarations": declarations},
			Human: strings.Join(names, "\n"),
		}
	case "show":
		if len(rest) != 2 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest declaration show <name> [--json]"}
		}
		name := rest[1]
		declaration, loadErr := loadDeclaration(flags.root, name)
		if loadErr != nil {
			if _, known := cfg.Agents[name]; !known {
				return cliOutcome{Exit: exitNotFound, ErrText: fmt.Sprintf("declaration %q not found", name)}
			}
			return cliOutcome{Exit: exitError, ErrText: loadErr.Error()}
		}
		human := fmt.Sprintf("declaration %s\nmodel: %s\ntools: %s\nthinking: %s\nsystem_prompt:\n%s\ntask_prompt:\n%s",
			declaration.Name, declaration.Model, strings.Join(declaration.Tools, ","), declaration.Thinking,
			declaration.SystemPrompt, declaration.TaskPrompt)
		return cliOutcome{Exit: exitOK, JSON: map[string]any{"declaration": declaration}, Human: human}
	default:
		return cliOutcome{Exit: exitInvalidArg, ErrText: fmt.Sprintf("unknown declaration command %q", rest[0])}
	}
}

func runTrigger(rest []string, flags cliFlags) cliOutcome {
	if len(rest) == 0 {
		return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest trigger list|show <agent>|reset <agent> [--json]"}
	}
	health, err := readTriggerHealth(flags.root)
	if err != nil {
		return cliOutcome{Exit: exitError, ErrText: err.Error()}
	}
	if health == nil {
		health = map[string]TriggerHealth{}
	}
	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest trigger list [--json]"}
		}
		return cliOutcome{Exit: exitOK, JSON: map[string]any{"triggers": health}, Human: triggerHealthHuman(health)}
	case "show":
		if len(rest) != 2 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest trigger show <agent> [--json]"}
		}
		value, ok := health[rest[1]]
		if !ok {
			return cliOutcome{Exit: exitNotFound, ErrText: fmt.Sprintf("trigger %q not found", rest[1])}
		}
		return cliOutcome{Exit: exitOK, JSON: map[string]any{"trigger": value}, Human: triggerHealthHuman(map[string]TriggerHealth{rest[1]: value})}
	case "reset":
		if len(rest) != 2 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest trigger reset <agent>"}
		}
		agent := rest[1]
		value, ok := health[agent]
		if !ok {
			return cliOutcome{Exit: exitNotFound, ErrText: fmt.Sprintf("trigger %q not found", agent)}
		}
		value.ConsecutiveErrors = 0
		value.PollError = ""
		value.RunError = ""
		value.AuditError = ""
		health[agent] = value
		if err := writeTriggerHealth(flags.root, health); err != nil {
			return cliOutcome{Exit: exitError, ErrText: fmt.Errorf("persist trigger state: %w", err).Error()}
		}
		return cliOutcome{Exit: exitOK, JSON: map[string]any{"trigger": value}, Human: triggerHealthHuman(map[string]TriggerHealth{agent: value})}
	default:
		return cliOutcome{Exit: exitInvalidArg, ErrText: fmt.Sprintf("unknown trigger command %q", rest[0])}
	}
}

func triggerHealthHuman(health map[string]TriggerHealth) string {
	names := slicesSortedKeys(health)
	var human strings.Builder
	human.WriteString("triggers:")
	for _, name := range names {
		value := health[name]
		human.WriteString(fmt.Sprintf("\n  %s errors=%d code=%d running=%t", name, value.ConsecutiveErrors, value.LastCode, value.Running))
		for _, field := range []struct {
			label string
			value string
		}{
			{"poll_error", value.PollError},
			{"run_error", value.RunError},
			{"audit_error", value.AuditError},
		} {
			if field.value != "" {
				human.WriteString(fmt.Sprintf(" %s=%s", field.label, field.value))
			}
		}
	}
	return human.String()
}

func slicesSortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for index := 1; index < len(keys); index++ {
		for current := index; current > 0 && keys[current] < keys[current-1]; current-- {
			keys[current], keys[current-1] = keys[current-1], keys[current]
		}
	}
	return keys
}

func writeTriggerHealth(root string, health map[string]TriggerHealth) error {
	dir := filepath.Join(root, workspaceName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(health, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "triggers.json.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, "triggers.json"))
}

func runRun(rest []string, flags cliFlags) cliOutcome {
	if len(rest) == 0 {
		return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest run list|show <run-id>|logs [--follow] <run-id> [--json]"}
	}
	switch rest[0] {
	case "list":
		if len(rest) != 1 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest run list [--limit N] [--after <run-id>] [--json]"}
		}
		return runList(flags)
	case "show":
		if len(rest) != 2 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest run show <run-id> [--json]"}
		}
		record, err := findRunRecord(flags.root, rest[1])
		if err != nil {
			return cliOutcome{Exit: exitError, ErrText: err.Error()}
		}
		if record == nil {
			return cliOutcome{Exit: exitNotFound, ErrText: fmt.Sprintf("run %q not found", rest[1])}
		}
		human := fmt.Sprintf("%s\tagent=%s\texit=%d\tduration=%.3f\tstarted=%s",
			record.RunID, record.Agent, record.Exit, record.Duration, record.Started)
		return cliOutcome{Exit: exitOK, JSON: map[string]any{"run": record}, Human: human}
	case "logs":
		if len(rest) != 2 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest run logs [--follow] <run-id>"}
		}
		if flags.follow && flags.json {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "--follow streams raw log bytes and is incompatible with --json"}
		}
		return runLogs(flags.root, rest[1], flags.follow)
	default:
		return cliOutcome{Exit: exitInvalidArg, ErrText: fmt.Sprintf("unknown run command %q", rest[0])}
	}
}

func runList(flags cliFlags) cliOutcome {
	records, err := ReadLedger(flags.root)
	if err != nil {
		return cliOutcome{Exit: exitError, ErrText: err.Error()}
	}
	page, nextAfter := pageRuns(records, flags.limit, flags.after)
	var human strings.Builder
	if len(page) == 0 {
		human.WriteString("no runs")
	} else {
		for _, record := range page {
			human.WriteString(fmt.Sprintf("%s\tagent=%s\texit=%d\tduration=%.3f\n", record.RunID, record.Agent, record.Exit, record.Duration))
		}
	}
	return cliOutcome{
		Exit:  exitOK,
		JSON:  map[string]any{"runs": page, "next_after": nextAfter},
		Human: strings.TrimSuffix(human.String(), "\n"),
	}
}

// pageRuns returns the latest-first window of the ledger. The ledger file is
// append-order, so "latest first" walks it backward. --after names the newest
// run identity already seen by the caller; the next page starts after it.
func pageRuns(records []RunRecord, limit int, after string) ([]RunRecord, string) {
	latestFirst := make([]RunRecord, len(records))
	for index := range records {
		latestFirst[len(records)-1-index] = records[index]
	}
	start := 0
	if after != "" {
		for index, record := range latestFirst {
			if record.RunID == after {
				start = index + 1
				break
			}
		}
	}
	end := len(latestFirst)
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	page := latestFirst[start:end]
	nextAfter := ""
	if end < len(latestFirst) && len(page) > 0 {
		nextAfter = page[len(page)-1].RunID
	}
	return page, nextAfter
}

func findRunRecord(root, runID string) (*RunRecord, error) {
	records, err := ReadLedger(root)
	if err != nil {
		return nil, err
	}
	for index := range records {
		if records[index].RunID == runID {
			return &records[index], nil
		}
	}
	return nil, nil
}

func runLogs(root, runID string, follow bool) cliOutcome {
	logPath := forestPath(root, "runs", runID+".log")
	if !follow {
		record, err := findRunRecord(root, runID)
		if err != nil {
			return cliOutcome{Exit: exitError, ErrText: err.Error()}
		}
		human, readErr := readRunLog(logPath)
		if readErr != nil {
			if record == nil {
				return cliOutcome{Exit: exitNotFound, ErrText: fmt.Sprintf("run %q not found", runID)}
			}
			return cliOutcome{Exit: exitOK, Human: "log not retained"} // retention evicted it; the run still exists
		}
		return cliOutcome{Exit: exitOK, Human: strings.TrimSuffix(human, "\n")}
	}
	return followRunLog(root, runID, logPath)
}

// followRunLog streams the log until the Run completes, then exits with the
// Run's recorded exit code. A completed Run returns immediately with that code.
func followRunLog(root, runID, logPath string) cliOutcome {
	if record, err := findRunRecord(root, runID); record != nil || err != nil {
		if err != nil {
			return cliOutcome{Exit: exitError, ErrText: err.Error()}
		}
		human, readErr := readRunLog(logPath)
		if readErr != nil {
			return cliOutcome{Exit: record.Exit, Human: "log not retained"}
		}
		return cliOutcome{Exit: record.Exit, Human: human}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	offset := int64(0)
	for {
		human, readErr := readRunLogFrom(logPath, offset)
		if readErr == nil {
			fmt.Print(human)
			offset += int64(len(human))
		}
		record, findErr := findRunRecord(root, runID)
		if findErr != nil {
			return cliOutcome{Exit: exitError, ErrText: findErr.Error()}
		}
		if record != nil {
			tail, tailErr := readRunLogFrom(logPath, offset)
			if tailErr == nil {
				fmt.Print(tail)
			}
			return cliOutcome{Exit: record.Exit}
		}
		select {
		case <-ctx.Done():
			return cliOutcome{Exit: exitNoWork, ErrText: "interrupted"}
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func readRunLog(path string) (string, error) {
	return readRunLogFrom(path, 0)
}

func readRunLogFrom(path string, offset int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func runAudit(rest []string, flags cliFlags) cliOutcome {
	if len(rest) == 0 {
		return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest audit show [--rescan]|log [--limit N] [--json]"}
	}
	switch rest[0] {
	case "show":
		if len(rest) != 1 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest audit show [--rescan] [--json]"}
		}
		if flags.rescan {
			result, rescanErr := audit(context.Background(), flags.root)
			if rescanErr != nil {
				return cliOutcome{Exit: exitError, ErrText: rescanErr.Error()}
			}
			human := fmt.Sprintf("last audit: %s master=%s violations=%d", result.Master, result.Master, len(result.Violations))
			return cliOutcome{
				Exit:  exitOK,
				JSON:  map[string]any{"master": result.Master, "violations": result.Violations},
				Human: human,
			}
		}
		state, err := readAuditState(flags.root)
		if err != nil {
			return cliOutcome{Exit: exitError, ErrText: err.Error()}
		}
		return cliOutcome{Exit: exitOK, JSON: map[string]any{"audit": state}, Human: auditStateHuman(state)}
	case "log":
		if len(rest) != 1 {
			return cliOutcome{Exit: exitInvalidArg, ErrText: "usage: forest audit log [--limit N] [--json]"}
		}
		entries, err := readAuditLogEntries(flags.root)
		if err != nil {
			return cliOutcome{Exit: exitError, ErrText: err.Error()}
		}
		limit := flags.limit
		if limit <= 0 {
			limit = 1000
		}
		entries = latestAuditEntries(entries, limit)
		return cliOutcome{
			Exit:  exitOK,
			JSON:  map[string]any{"entries": entries},
			Human: strings.Join(entries, "\n"),
		}
	default:
		return cliOutcome{Exit: exitInvalidArg, ErrText: fmt.Sprintf("unknown audit command %q", rest[0])}
	}
}

func auditStateHuman(state AuditState) string {
	var human strings.Builder
	human.WriteString(fmt.Sprintf("last audit: %s master=%s", state.LastResult, state.LastMaster))
	human.WriteString(fmt.Sprintf("\naudit violations: total=%d", len(state.Violations)))
	for _, violation := range state.Violations {
		human.WriteString(fmt.Sprintf("\naudit violation: %s", violation))
	}
	return human.String()
}

func readAuditLogEntries(root string) ([]string, error) {
	var entries []string
	err := scanAuditLog(context.Background(), auditLogPath(root), func(entry string) {
		entries = append(entries, entry)
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func latestAuditEntries(entries []string, limit int) []string {
	if len(entries) <= limit {
		return entries
	}
	return entries[len(entries)-limit:]
}
