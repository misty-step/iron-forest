package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Stable process exit codes. The usage text publishes these, so they are part of
// the CLI contract.
const (
	exitOK         = 0
	exitNoWork     = 1
	exitError      = 2
	exitNotFound   = 4
	exitConflict   = 5
	exitInvalidArg = 6
)

// defaultRunPage bounds `run list` when the caller names no limit, so the
// paging cursor always has a page to continue from.
const defaultRunPage = 50

// followPollInterval is how often `run logs --follow` re-reads the log and the
// ledger while waiting for a Run to finish.
const followPollInterval = 500 * time.Millisecond

// cliEnvelope is the one machine-readable shape every --json command emits.
type cliEnvelope struct {
	Schema  string   `json:"schema"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
	Exit    int      `json:"exit"`
	Data    any      `json:"data"`
	Error   *string  `json:"error"`
}

// cliOutcome is the normalized result of one command. A command fills Data plus
// Human, or Stream when it owns its own bytes, or ErrText when it fails.
type cliOutcome struct {
	Exit    int
	Data    any
	Human   string
	ErrText string
	Stream  func(io.Writer) int
}

func failure(exit int, format string, args ...any) cliOutcome {
	return cliOutcome{Exit: exit, ErrText: fmt.Sprintf(format, args...)}
}

// optionalFlag names a per-command flag beyond the universal --json and --root.
type optionalFlag uint8

const (
	flagLimit optionalFlag = 1 << iota
	flagAfter
	flagFollow
	flagRescan
)

type cliFlags struct {
	root   string
	json   bool
	limit  int
	after  string
	follow bool
	rescan bool
}

// set reports which optional flags the caller actually passed, so a command can
// reject the ones it does not implement instead of ignoring them.
func (f cliFlags) set() optionalFlag {
	var bits optionalFlag
	if f.limit != 0 {
		bits |= flagLimit
	}
	if f.after != "" {
		bits |= flagAfter
	}
	if f.follow {
		bits |= flagFollow
	}
	if f.rescan {
		bits |= flagRescan
	}
	return bits
}

func (bits optionalFlag) names() []string {
	var names []string
	for _, candidate := range []struct {
		bit  optionalFlag
		name string
	}{{flagLimit, "--limit"}, {flagAfter, "--after"}, {flagFollow, "--follow"}, {flagRescan, "--rescan"}} {
		if bits&candidate.bit != 0 {
			names = append(names, candidate.name)
		}
	}
	return names
}

// cliCommand is one row of the read surface. The table is the only statement of
// the grammar: dispatch, arity, accepted flags, and usage text all read from it.
type cliCommand struct {
	phrase   string
	args     int
	operands string
	optional optionalFlag
	run      func(rest []string, flags cliFlags) cliOutcome
}

func cliCommands() []cliCommand {
	return []cliCommand{
		{phrase: "status", run: runStatus},
		{phrase: "selfcheck", run: runSelfcheck},
		{phrase: "config show", run: runConfigShow},
		{phrase: "declaration list", run: runDeclarationList},
		{phrase: "declaration show", args: 1, operands: "<name>", run: runDeclarationShow},
		{phrase: "trigger list", run: runTriggerList},
		{phrase: "trigger show", args: 1, operands: "<agent>", run: runTriggerShow},
		{phrase: "trigger reset", args: 1, operands: "<agent>", run: runTriggerReset},
		{phrase: "run list", optional: flagLimit | flagAfter, run: runRunList},
		{phrase: "run show", args: 1, operands: "<run-id>", run: runRunShow},
		{phrase: "run logs", args: 1, operands: "<run-id>", optional: flagFollow, run: runRunLogs},
		{phrase: "audit show", optional: flagRescan, run: runAuditShow},
		{phrase: "audit log", optional: flagLimit, run: runAuditLog},
	}
}

func (c cliCommand) usage() string {
	usage := "forest " + c.phrase
	if c.operands != "" {
		usage += " " + c.operands
	}
	for _, name := range c.optional.names() {
		usage += " [" + name + "]"
	}
	return usage + " [--json] [--root <dir>]"
}

// runSurfaceCommand parses, dispatches, and renders one read-surface command.
func runSurfaceCommand(args []string) int {
	positional, flags, err := parseCLIFlags(args)
	if err != nil {
		return render(args[0], nil, flags, failure(exitInvalidArg, "%s", err))
	}
	command, rest, ok := lookupCommand(positional)
	if !ok {
		phrase := strings.Join(positional, " ")
		code := render(phrase, nil, flags, failure(exitInvalidArg, "unknown command %q", phrase))
		if !flags.json {
			printUsage()
		}
		return code
	}
	if len(rest) != command.args {
		return render(command.phrase, rest, flags, failure(exitInvalidArg, "usage: %s", command.usage()))
	}
	if rejected := flags.set() & ^command.optional; rejected != 0 {
		return render(command.phrase, rest, flags, failure(exitInvalidArg,
			"%s does not accept %s", command.phrase, strings.Join(rejected.names(), " ")))
	}
	return render(command.phrase, rest, flags, command.run(rest, flags))
}

// lookupCommand resolves the longest matching phrase, so "run" and "run logs"
// cannot disagree about which row owns the call.
func lookupCommand(positional []string) (cliCommand, []string, bool) {
	table := cliCommands()
	for width := min(2, len(positional)); width >= 1; width-- {
		phrase := strings.Join(positional[:width], " ")
		if index := slices.IndexFunc(table, func(c cliCommand) bool { return c.phrase == phrase }); index >= 0 {
			return table[index], positional[width:], true
		}
	}
	return cliCommand{}, nil, false
}

// render is the single place a command's result reaches the process boundary.
func render(command string, args []string, flags cliFlags, outcome cliOutcome) int {
	if flags.json {
		if args == nil {
			args = []string{}
		}
		var errText *string
		if outcome.ErrText != "" {
			errText = &outcome.ErrText
		}
		envelope := cliEnvelope{
			Schema:  "forest.cli.v1",
			Command: command,
			Args:    args,
			Exit:    outcome.Exit,
			Data:    outcome.Data,
			Error:   errText,
		}
		if err := json.NewEncoder(os.Stdout).Encode(envelope); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitError
		}
		return outcome.Exit
	}
	if outcome.ErrText != "" {
		fmt.Fprintln(os.Stderr, outcome.ErrText)
		return outcome.Exit
	}
	if outcome.Stream != nil {
		return outcome.Stream(os.Stdout)
	}
	if outcome.Human != "" {
		fmt.Println(outcome.Human)
	}
	return outcome.Exit
}

// wantsJSON answers before parsing succeeds, so a rejected flag still reports
// through the envelope rather than as loose stderr text.
func wantsJSON(args []string) bool { return slices.Contains(args, "--json") }

func parseCLIFlags(args []string) ([]string, cliFlags, error) {
	flags := cliFlags{json: wantsJSON(args)}
	var positional []string
	value := func(index int, name string) (string, int, error) {
		if arg := args[index]; strings.HasPrefix(arg, name+"=") {
			return strings.TrimPrefix(arg, name+"="), index, nil
		}
		if index+1 >= len(args) {
			return "", index, fmt.Errorf("%s requires a value", name)
		}
		return args[index+1], index + 1, nil
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--json":
			flags.json = true
		case "--follow":
			flags.follow = true
		case "--rescan":
			flags.rescan = true
		case "--root", "-C":
			root, next, err := value(index, name)
			if err != nil {
				return nil, flags, err
			}
			flags.root, index = root, next
		case "--after":
			after, next, err := value(index, name)
			if err != nil {
				return nil, flags, err
			}
			flags.after, index = after, next
		case "--limit":
			raw, next, err := value(index, name)
			if err != nil {
				return nil, flags, err
			}
			limit, convErr := strconv.Atoi(raw)
			if convErr != nil || limit <= 0 {
				return nil, flags, fmt.Errorf("--limit must be a positive integer, got %q", raw)
			}
			flags.limit, index = limit, next
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, flags, fmt.Errorf("unknown flag %q", arg)
			}
			positional = append(positional, arg)
		}
	}
	if flags.root == "" {
		root, err := os.Getwd()
		if err != nil {
			return nil, flags, fmt.Errorf("resolve working directory: %w", err)
		}
		flags.root = root
	}
	return positional, flags, nil
}

func runConfigShow(_ []string, flags cliFlags) cliOutcome {
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return failure(exitError, "%s", err)
	}
	human := fmt.Sprintf("repo: %s", cfg.Repo)
	for _, name := range agentNames(cfg) {
		agent := cfg.Agents[name]
		human += fmt.Sprintf("\nagent %s: poll=%q interval=%ds timeout=%ds", name, agent.Poll, agent.Interval, agent.Timeout)
	}
	for _, check := range cfg.Checks {
		human += fmt.Sprintf("\ncheck %s: run=%q", check.Name, check.Run)
	}
	return cliOutcome{Exit: exitOK, Data: cfg, Human: human}
}

type declarationListPayload struct {
	Declarations []Declaration `json:"declarations"`
}

func runDeclarationList(_ []string, flags cliFlags) cliOutcome {
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return failure(exitError, "%s", err)
	}
	names := agentNames(cfg)
	declarations := make([]Declaration, 0, len(names))
	for _, name := range names {
		declaration, loadErr := loadDeclaration(flags.root, name)
		if loadErr != nil {
			return failure(exitError, "%s", loadErr)
		}
		declarations = append(declarations, declaration)
	}
	return cliOutcome{
		Exit:  exitOK,
		Data:  declarationListPayload{Declarations: declarations},
		Human: strings.Join(names, "\n"),
	}
}

func runDeclarationShow(rest []string, flags cliFlags) cliOutcome {
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return failure(exitError, "%s", err)
	}
	name := rest[0]
	declaration, loadErr := loadDeclaration(flags.root, name)
	if loadErr != nil {
		if _, configured := cfg.Agents[name]; !configured {
			return failure(exitNotFound, "declaration %q not found", name)
		}
		return failure(exitError, "%s", loadErr)
	}
	human := fmt.Sprintf("declaration %s\nmodel: %s\ntools: %s\nthinking: %s\nsystem_prompt:\n%s\ntask_prompt:\n%s",
		declaration.Name, declaration.Model, strings.Join(declaration.Tools, ","), declaration.Thinking,
		declaration.SystemPrompt, declaration.TaskPrompt)
	return cliOutcome{Exit: exitOK, Data: declaration, Human: human}
}

type triggerListPayload struct {
	Triggers  []TriggerView `json:"triggers"`
	StateErr  string        `json:"state_error,omitempty"`
	StateRead bool          `json:"state_written"`
}

func runTriggerList(_ []string, flags cliFlags) cliOutcome {
	state, err := resolveTriggerState(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	payload := triggerListPayload{Triggers: state.Views, StateRead: state.StateRead}
	if state.StateErr != nil {
		payload.StateErr = state.StateErr.Error()
	}
	return cliOutcome{Exit: exitOK, Data: payload, Human: triggerViewsHuman(state.Views)}
}

func runTriggerShow(rest []string, flags cliFlags) cliOutcome {
	state, err := resolveTriggerState(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	index := slices.IndexFunc(state.Views, func(view TriggerView) bool { return view.Name == rest[0] })
	if index < 0 {
		return failure(exitNotFound, "trigger %q not found", rest[0])
	}
	return cliOutcome{Exit: exitOK, Data: state.Views[index], Human: triggerViewsHuman(state.Views[index : index+1])}
}

// runTriggerReset clears one agent's accumulated errors. The Scheduler owns this
// file while it runs, so the command refuses rather than racing it.
func runTriggerReset(rest []string, flags cliFlags) cliOutcome {
	agent := rest[0]
	held, err := kernelLockHeld(flags.root)
	if err != nil {
		return failure(exitError, "kernel lock state unknown: %s", err)
	}
	if held {
		return failure(exitConflict, "a Kernel is running; stop it before resetting %q", agent)
	}
	health, exists, err := readTriggerHealth(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	value, present := health[agent]
	if !exists || !present {
		return failure(exitNotFound, "trigger %q not found", agent)
	}
	value.ConsecutiveErrors = 0
	value.PollError = ""
	value.RunError = ""
	value.AuditError = ""
	health[agent] = value
	if err := writeTriggerHealth(flags.root, health); err != nil {
		return failure(exitError, "persist trigger state: %s", err)
	}
	state, err := resolveTriggerState(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	index := slices.IndexFunc(state.Views, func(view TriggerView) bool { return view.Name == agent })
	if index < 0 {
		return failure(exitError, "trigger %q vanished during reset", agent)
	}
	return cliOutcome{Exit: exitOK, Data: state.Views[index], Human: triggerViewsHuman(state.Views[index : index+1])}
}

type runListPayload struct {
	Runs      []RunRecord `json:"runs"`
	NextAfter string      `json:"next_after"`
}

func runRunList(_ []string, flags cliFlags) cliOutcome {
	limit := flags.limit
	if limit <= 0 {
		limit = defaultRunPage
	}
	records, nextAfter, err := ReadLedgerPage(flags.root, limit, flags.after)
	if err != nil {
		if errors.Is(err, errLedgerCursorUnknown) {
			return failure(exitNotFound, "%s", err)
		}
		return failure(exitError, "%s", err)
	}
	human := "no runs"
	if len(records) > 0 {
		human = runRecordsHuman(records, "")
	}
	return cliOutcome{Exit: exitOK, Data: runListPayload{Runs: records, NextAfter: nextAfter}, Human: human}
}

func runRunShow(rest []string, flags cliFlags) cliOutcome {
	record, found, err := FindRun(flags.root, rest[0])
	if err != nil {
		return failure(exitError, "%s", err)
	}
	if !found {
		return failure(exitNotFound, "run %q not found", rest[0])
	}
	human := fmt.Sprintf("%s\tagent=%s\texit=%d\tduration=%.3f\tstarted=%s",
		record.RunID, record.Agent, record.Exit, record.Duration, record.Started)
	return cliOutcome{Exit: exitOK, Data: record, Human: human}
}

type runLogPayload struct {
	RunID    string `json:"run_id"`
	Retained bool   `json:"retained"`
	Complete bool   `json:"complete"`
	Exit     int    `json:"exit"`
	Text     string `json:"text"`
}

func runRunLogs(rest []string, flags cliFlags) cliOutcome {
	runID := rest[0]
	if flags.follow && flags.json {
		return failure(exitInvalidArg, "--follow streams the live log and cannot emit one envelope; drop --json")
	}
	logPath := runLogPath(flags.root, runID)
	record, found, err := FindRun(flags.root, runID)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	text, readErr := readRunLogFrom(logPath, 0)
	// A run identity is real when the ledger has it or its log exists; --follow
	// on anything else would wait for a Run that will never appear.
	if !found && readErr != nil {
		return failure(exitNotFound, "run %q not found", runID)
	}
	if flags.follow && !found {
		return cliOutcome{Exit: exitOK, Stream: func(w io.Writer) int { return followRunLog(w, flags.root, runID, logPath) }}
	}
	retained := readErr == nil
	if !retained && !os.IsNotExist(readErr) {
		return failure(exitError, "%s", readErr)
	}
	payload := runLogPayload{RunID: runID, Retained: retained, Complete: found, Exit: record.Exit, Text: text}
	human := text
	if !retained {
		human = "log not retained"
	}
	exit := exitOK
	if flags.follow {
		// The Run already finished, so follow has nothing to wait for and
		// reports the Run's own outcome.
		exit = record.Exit
	}
	return cliOutcome{Exit: exit, Data: payload, Human: strings.TrimSuffix(human, "\n")}
}

// followRunLog streams new log bytes until the Run's ledger row appears, then
// exits with the Run's recorded exit code.
func followRunLog(w io.Writer, root, runID, logPath string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	offset := int64(0)
	for {
		// The ledger is read first so the log read that follows cannot miss
		// bytes written between the two.
		record, found, err := FindRun(root, runID)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return exitError
		}
		if chunk, readErr := readRunLogFrom(logPath, offset); readErr == nil {
			fmt.Fprint(w, chunk)
			offset += int64(len(chunk))
		}
		if found {
			return record.Exit
		}
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "interrupted")
			return exitError
		case <-time.After(followPollInterval):
		}
	}
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

func runAuditShow(_ []string, flags cliFlags) cliOutcome {
	if flags.rescan {
		// audit() persists the state it computes, so the read below reports the
		// rescan. Refuse while a Kernel owns the audit files.
		held, err := kernelLockHeld(flags.root)
		if err != nil {
			return failure(exitError, "kernel lock state unknown: %s", err)
		}
		if held {
			return failure(exitConflict, "a Kernel is running; it audits on its own")
		}
		if _, err := audit(context.Background(), flags.root); err != nil {
			return failure(exitError, "%s", err)
		}
	}
	state, err := readAuditState(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	return cliOutcome{Exit: exitOK, Data: state, Human: auditStateHuman(state, len(state.Violations))}
}

type auditLogPayload struct {
	Entries []string `json:"entries"`
}

func runAuditLog(_ []string, flags cliFlags) cliOutcome {
	entries, err := ReadAuditLog(context.Background(), flags.root, flags.limit)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	return cliOutcome{Exit: exitOK, Data: auditLogPayload{Entries: entries}, Human: strings.Join(entries, "\n")}
}

// auditStateHuman renders audit state, showing at most cap violations and
// reporting the remainder as a count.
func auditStateHuman(state AuditState, cap int) string {
	shown := min(len(state.Violations), cap)
	var human strings.Builder
	fmt.Fprintf(&human, "last audit: %s master=%s\n", state.LastResult, state.LastMaster)
	fmt.Fprintf(&human, "audit violations: total=%d", len(state.Violations))
	if omitted := len(state.Violations) - shown; omitted > 0 {
		fmt.Fprintf(&human, " omitted=%d", omitted)
	}
	for _, violation := range state.Violations[:shown] {
		fmt.Fprintf(&human, "\naudit violation: %s", violation)
	}
	return human.String()
}

func runRecordsHuman(records []RunRecord, indent string) string {
	rows := make([]string, 0, len(records))
	for _, record := range records {
		rows = append(rows, fmt.Sprintf("%s%s agent=%s exit=%d duration=%.3f",
			indent, record.RunID, record.Agent, record.Exit, record.Duration))
	}
	return strings.Join(rows, "\n")
}
