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
	"unicode"
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
// Human, or Stream when it owns its own bytes, or ErrText when it fails. Warning
// is a reason the answer is partial: it reaches stderr in human mode, and under
// --json the payload already carries it, so it is not repeated.
type cliOutcome struct {
	Exit    int
	Data    any
	Human   string
	Warning string
	ErrText string
	Stream  func(io.Writer) int
}

func failure(exit int, format string, args ...any) cliOutcome {
	return cliOutcome{Exit: exit, ErrText: fmt.Sprintf(format, args...)}
}

// Optional flag names. --json and --root are universal; every other flag is
// declared per command so an unsupported flag is an error, not a no-op.
const (
	flagLimit  = "--limit"
	flagAfter  = "--after"
	flagFollow = "--follow"
	flagRescan = "--rescan"
)

type cliFlags struct {
	root   string
	json   bool
	limit  int
	after  string
	follow bool
	rescan bool
	// seen records the optional flags the caller actually passed. Presence is
	// recorded here rather than inferred from values, so an empty value cannot
	// slip past a command's allowlist.
	seen []string
}

// cliCommand is one row of the read surface. The table is the only statement of
// the grammar: dispatch, arity, accepted flags, and usage text all read from it.
type cliCommand struct {
	phrase   string
	args     int
	operands string
	optional []string
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
		{phrase: "run list", optional: []string{flagLimit, flagAfter}, run: runRunList},
		{phrase: "run show", args: 1, operands: "<run-id>", run: runRunShow},
		{phrase: "run logs", args: 1, operands: "<run-id>", optional: []string{flagFollow}, run: runRunLogs},
		{phrase: "audit show", optional: []string{flagRescan}, run: runAuditShow},
		{phrase: "audit log", optional: []string{flagLimit}, run: runAuditLog},
	}
}

// rejects lists the passed flags this command does not implement.
func (c cliCommand) rejects(flags cliFlags) []string {
	var rejected []string
	for _, name := range flags.seen {
		if !slices.Contains(c.optional, name) {
			rejected = append(rejected, name)
		}
	}
	return rejected
}

// usage states the command's grammar. --follow is exclusive with --json, so the
// two forms are written separately rather than as one line the binary rejects.
func (c cliCommand) usage() string {
	head := "forest " + c.phrase
	if c.operands != "" {
		head += " " + c.operands
	}
	if slices.Contains(c.optional, flagFollow) {
		return head + " [--json] [--root <dir>], or " + head + " --follow [--root <dir>]"
	}
	for _, name := range c.optional {
		head += " [" + name + "]"
	}
	return head + " [--json] [--root <dir>]"
}

// runSurfaceCommand parses, dispatches, and renders one read-surface command.
func runSurfaceCommand(args []string) int {
	positional, flags, err := parseCLIFlags(args)
	if err != nil {
		// Name the command as far as it parsed, so the envelope still says which
		// command was refused.
		phrase := ""
		if command, _, ok := lookupCommand(positional); ok {
			phrase = command.phrase
		}
		return render(phrase, nil, flags, failure(exitInvalidArg, "%s", err))
	}
	command, rest, ok := lookupCommand(positional)
	if !ok {
		phrase := strings.Join(positional, " ")
		outcome := failure(exitInvalidArg, "unknown command %q", phrase)
		// A group name is not unknown: it is incomplete. Naming its subcommands
		// answers the operator instead of contradicting the usage block below.
		if subcommands := subcommandsOf(positional); len(subcommands) > 0 {
			outcome = failure(exitInvalidArg, "%s requires a subcommand: %s",
				phrase, strings.Join(subcommands, ", "))
		}
		code := render(phrase, nil, flags, outcome)
		if !flags.json {
			printUsage()
		}
		return code
	}
	if len(rest) != command.args {
		complaint := "was given too many operands"
		if len(rest) < command.args {
			complaint = "needs an operand"
		}
		return render(command.phrase, rest, flags, failure(exitInvalidArg,
			"%s %s; usage: %s", command.phrase, complaint, command.usage()))
	}
	if rejected := command.rejects(flags); len(rejected) > 0 {
		return render(command.phrase, rest, flags, failure(exitInvalidArg,
			"%s does not accept %s; usage: %s",
			command.phrase, strings.Join(rejected, " "), command.usage()))
	}
	// Every command reads one checkout. Answering from a directory that holds no
	// configuration would report a clean factory where there is none.
	if _, err := os.Stat(configPath(flags.root)); err != nil {
		return render(command.phrase, rest, flags, failure(exitError,
			"%s is not an Iron Forest checkout: no forest.yaml", flags.root))
	}
	return render(command.phrase, rest, flags, command.run(rest, flags))
}

// subcommandsOf names the rows under a group phrase, so an incomplete command
// reports its options instead of being called unknown.
func subcommandsOf(positional []string) []string {
	if len(positional) != 1 {
		return nil
	}
	var subcommands []string
	for _, command := range cliCommands() {
		if group, sub, found := strings.Cut(command.phrase, " "); found && group == positional[0] {
			subcommands = append(subcommands, sub)
		}
	}
	return subcommands
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
	// A warning explains a partial answer, so it must not land in the stream the
	// payload uses.
	if outcome.Warning != "" {
		fmt.Fprintln(os.Stderr, outcome.Warning)
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
// through the envelope rather than as loose stderr text. A malformed `--json=x`
// counts: the caller asked for the machine surface, so its refusal belongs
// there too.
func wantsJSON(args []string) bool {
	return slices.ContainsFunc(args, func(arg string) bool {
		name, _, _ := strings.Cut(arg, "=")
		return name == "--json"
	})
}

// parseCLIFlags splits flags from positionals. It returns the positionals it had
// collected even on failure, so the caller can still name the command.
func parseCLIFlags(args []string) ([]string, cliFlags, error) {
	flags := cliFlags{json: wantsJSON(args)}
	var positional []string
	// value reads a flag's argument from either `--name=value` or `--name value`.
	// A named identity may not be empty: an empty cursor or root would otherwise
	// read as "no flag at all" and bypass the command's allowlist.
	value := func(index int, name string) (string, int, error) {
		arg := args[index]
		if raw, ok := strings.CutPrefix(arg, name+"="); ok {
			if raw == "" {
				return "", index, fmt.Errorf("%s requires a value", name)
			}
			return raw, index, nil
		}
		if index+1 >= len(args) || args[index+1] == "" {
			return "", index, fmt.Errorf("%s requires a value", name)
		}
		return args[index+1], index + 1, nil
	}
	for index := 0; index < len(args); index++ {
		arg := args[index]
		name, _, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--json", flagFollow, flagRescan:
			if hasValue {
				return positional, flags, fmt.Errorf("%s does not take a value", name)
			}
			switch name {
			case "--json":
				flags.json = true
			case flagFollow:
				flags.follow = true
			default:
				flags.rescan = true
			}
			if name != "--json" {
				flags.seen = append(flags.seen, name)
			}
		case "--root", "-C":
			root, next, err := value(index, name)
			if err != nil {
				return positional, flags, err
			}
			flags.root, index = root, next
		case flagAfter:
			after, next, err := value(index, name)
			if err != nil {
				return positional, flags, err
			}
			flags.after, index = after, next
			flags.seen = append(flags.seen, flagAfter)
		case flagLimit:
			raw, next, err := value(index, name)
			if err != nil {
				return positional, flags, err
			}
			limit, convErr := strconv.Atoi(raw)
			if convErr != nil || limit <= 0 {
				return positional, flags, fmt.Errorf("--limit must be a positive integer, got %q", raw)
			}
			flags.limit, index = limit, next
			flags.seen = append(flags.seen, flagLimit)
		default:
			if strings.HasPrefix(arg, "-") {
				return positional, flags, fmt.Errorf("unknown flag %q", arg)
			}
			positional = append(positional, arg)
		}
	}
	if flags.root == "" {
		root, err := os.Getwd()
		if err != nil {
			return positional, flags, fmt.Errorf("resolve working directory: %w", err)
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
			return failure(exitNotFound, "declaration %q not found; see forest declaration list", name)
		}
		return failure(exitError, "%s", loadErr)
	}
	// Field labels sit at column zero and prompt bodies are indented, so audited
	// prompt text cannot pose as a declaration field. An unset field carries no
	// trailing space. Declared environment prints its keys only: a value is the
	// Run's business, not the reader's.
	envKeys := make([]string, 0, len(declaration.Env))
	for key := range declaration.Env {
		envKeys = append(envKeys, key)
	}
	slices.Sort(envKeys)
	human := strings.Join([]string{
		"declaration " + oneLine(declaration.Name),
		field("model", oneLine(declaration.Model)),
		field("model_source", oneLine(declaration.ModelSource)),
		field("tools", strings.Join(declaration.Tools, ",")),
		field("thinking", oneLine(declaration.Thinking)),
		field("env", strings.Join(envKeys, ",")),
		field("profile", "\n"+indentBlock(strings.Join(declaration.ProfileFiles, "\n"))),
		field("system_prompt", "\n"+indentBlock(declaration.SystemPrompt)),
		field("task_prompt", "\n"+indentBlock(declaration.TaskPrompt)),
	}, "\n")
	return cliOutcome{Exit: exitOK, Data: declaration, Human: human}
}

// field renders one label, omitting the separator when the value is empty so no
// line ends in trailing whitespace.
func field(label, value string) string {
	switch {
	case strings.TrimLeft(value, "\n") == "":
		return label + ":"
	case strings.HasPrefix(value, "\n"):
		// A block value starts on the next line, so no separator belongs here.
		return label + ":" + value
	default:
		return label + ": " + value
	}
}

type triggerListPayload struct {
	Triggers     []TriggerView `json:"triggers"`
	StateErr     string        `json:"state_error,omitempty"`
	StatePresent bool          `json:"state_present"`
}

func runTriggerList(_ []string, flags cliFlags) cliOutcome {
	state, err := resolveTriggerState(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	payload := triggerListPayload{Triggers: state.Views, StatePresent: state.StatePresent}
	if state.StateErr != nil {
		payload.StateErr = state.StateErr.Error()
	}
	return cliOutcome{Exit: exitOK, Data: payload, Human: triggerViewsHuman(state.Views), Warning: stateWarning(state)}
}

func runTriggerShow(rest []string, flags cliFlags) cliOutcome {
	state, err := resolveTriggerState(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	index := slices.IndexFunc(state.Views, func(view TriggerView) bool { return view.Name == rest[0] })
	if index < 0 {
		return failure(exitNotFound, "trigger %q is not configured; see forest trigger list", rest[0])
	}
	payload := triggerShowPayload{TriggerView: state.Views[index]}
	if state.StateErr != nil {
		payload.StateErr = state.StateErr.Error()
	}
	return cliOutcome{
		Exit:    exitOK,
		Data:    payload,
		Human:   triggerViewsHuman(state.Views[index : index+1]),
		Warning: stateWarning(state),
	}
}

// triggerShowPayload carries one trigger plus the reason its state may be
// unknown, so one agent can be investigated without reading the whole list.
type triggerShowPayload struct {
	TriggerView
	StateErr string `json:"state_error,omitempty"`
}

// stateWarning is the reason trigger state is unknown, reported once by every
// command that renders a trigger.
func stateWarning(state triggerState) string {
	if state.StateErr == nil {
		return ""
	}
	return "trigger state unknown: " + state.StateErr.Error()
}

// runTriggerReset clears one agent's accumulated errors. The Scheduler owns this
// file while it runs, so the write happens under the Kernel lock.
func runTriggerReset(rest []string, flags cliFlags) cliOutcome {
	agent := rest[0]
	cfg, err := loadConfig(configPath(flags.root))
	if err != nil {
		return failure(exitError, "%s", err)
	}
	if _, configured := cfg.Agents[agent]; !configured {
		return failure(exitNotFound, "trigger %q is not configured", agent)
	}
	// The write happens under the Kernel lock; the view is resolved after it is
	// released. Resolving inside would read this command's own lock as a running
	// Kernel and report the agent as live.
	if outcome := withKernelLock(flags.root, func() cliOutcome {
		health, _, err := readTriggerHealth(flags.root)
		if err != nil {
			return failure(exitError, "%s", err)
		}
		value, present := health[agent]
		if !present {
			return failure(exitNoWork, "trigger %q has no persisted state to clear", agent)
		}
		value.ConsecutiveErrors = 0
		value.PollError = ""
		value.RunError = ""
		value.AuditError = ""
		health[agent] = value
		if err := writeTriggerHealth(flags.root, health); err != nil {
			return failure(exitError, "persist trigger state: %s", err)
		}
		return cliOutcome{Exit: exitOK}
	}); outcome.Exit != exitOK {
		return outcome
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
	switch {
	case len(records) > 0:
		human = runRecordsHuman(records, "")
		if nextAfter != "" {
			human += fmt.Sprintf("\nmore runs: forest run list --after %s", nextAfter)
		}
	case flags.after != "":
		// "no runs" would claim the factory never ran anything; this is the end
		// of a page walk.
		human = "no more runs after " + oneLine(flags.after)
	}
	return cliOutcome{Exit: exitOK, Data: runListPayload{Runs: records, NextAfter: nextAfter}, Human: human}
}

func runRunShow(rest []string, flags cliFlags) cliOutcome {
	record, found, err := FindRun(flags.root, rest[0])
	if err != nil {
		return failure(exitError, "%s", err)
	}
	if !found {
		// A log without a ledger row is a real state, and `run logs` serves it.
		// Saying only "not found" would make two commands disagree.
		if _, statErr := os.Stat(runLogPath(flags.root, rest[0])); statErr == nil {
			return failure(exitNotFound,
				"run %q has no ledger row yet; its log exists, see forest run logs %s", rest[0], rest[0])
		}
		return failure(exitNotFound, "run %q not found; see forest run list", rest[0])
	}
	human := fmt.Sprintf("%s agent=%s exit=%d duration=%.3fs started=%s\n"+
		"  tokens_in=%d tokens_out=%d cache_read=%d cache_write=%d reasoning=%d",
		oneLine(record.RunID), oneLine(record.Agent), record.Exit, record.Duration, oneLine(record.Started),
		record.TokensIn, record.TokensOut, record.CacheRead, record.CacheWrite, record.Reasoning)
	return cliOutcome{Exit: exitOK, Data: record, Human: human}
}

type runLogPayload struct {
	RunID    string `json:"run_id"`
	Retained bool   `json:"retained"`
	Complete bool   `json:"complete"`
	// Exit is absent while a Run is in flight: reporting 0 would read as success.
	Exit *int   `json:"exit"`
	Text string `json:"text"`
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
	// A run identity is real when the ledger has it or its log exists. Following
	// anything else would wait for a Run that will never appear. A log that is
	// not a regular file would block both reads forever, so it is refused here.
	info, statErr := os.Stat(logPath)
	if !found && statErr != nil {
		return failure(exitNotFound, "run %q not found", runID)
	}
	if statErr == nil && !info.Mode().IsRegular() {
		return failure(exitError, "run log for %q is not a regular file", runID)
	}
	if flags.follow && !found {
		return cliOutcome{Exit: exitOK, Stream: func(w io.Writer) int { return followRunLog(w, flags.root, runID, logPath) }}
	}
	text, readErr := readRunLogFrom(logPath, 0)
	retained := readErr == nil
	if !retained && !os.IsNotExist(readErr) {
		return failure(exitError, "%s", readErr)
	}
	payload := runLogPayload{RunID: runID, Retained: retained, Complete: found, Text: text}
	if found {
		payload.Exit = &record.Exit
	}
	human := strings.TrimSuffix(text, "\n")
	warning := ""
	if !retained {
		// A status sentence is not log content, so it must not land in the stream
		// a caller redirects into a file.
		human, warning = "", fmt.Sprintf("run %q has no retained log", runID)
	}
	exit := exitOK
	if flags.follow {
		// The Run already finished, so follow has nothing to wait for and
		// reports the Run's own outcome, as the command contract requires. The
		// warning states whose code it is, because that code overlaps the CLI's.
		exit = record.Exit
		warning = fmt.Sprintf("run %q exited %d", runID, record.Exit)
	}
	return cliOutcome{Exit: exit, Data: payload, Human: human, Warning: warning}
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
		// Every Run is dispatched under the Kernel lock and its ledger row is
		// written before that lock is released. A free lock plus a confirming
		// miss therefore proves the Run died without recording an outcome.
		if held, lockErr := kernelLockHeld(root); lockErr == nil && !held {
			if _, confirmed, findErr := FindRun(root, runID); findErr == nil && !confirmed {
				fmt.Fprintf(os.Stderr, "run %q did not complete and no Kernel is running\n", runID)
				return exitError
			}
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
		// audit() rewrites audit.json and audit.log, and its temp cleanup would
		// remove a concurrent writer's in-flight file, so it runs under the
		// Kernel lock. It persists the state the read below reports.
		if outcome := withKernelLock(flags.root, func() cliOutcome {
			if _, err := audit(context.Background(), flags.root); err != nil {
				return failure(exitError, "%s", err)
			}
			return cliOutcome{Exit: exitOK}
		}); outcome.ErrText != "" {
			return outcome
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
	human := "no audit history"
	if len(entries) > 0 {
		human = fmt.Sprintf("audit history: showing=%d\n%s", len(entries), strings.Join(entries, "\n"))
	}
	return cliOutcome{Exit: exitOK, Data: auditLogPayload{Entries: entries}, Human: human}
}

// oneLine keeps a stored value inside the line that reports it. Poll output, Run
// errors, Auditor violations, and ledger identities are agent-authored, so an
// embedded newline would otherwise forge additional lines in a human report.
func oneLine(value string) string {
	if strings.ContainsFunc(value, unicode.IsControl) {
		return strconv.Quote(value)
	}
	return value
}

// indentBlock indents a multi-line body so its text cannot pose as a field label
// at column zero.
func indentBlock(body string) string {
	trimmed := strings.TrimSuffix(body, "\n")
	if trimmed == "" {
		return ""
	}
	return "  " + strings.ReplaceAll(trimmed, "\n", "\n  ")
}

// auditStateHuman renders audit state, showing at most cap violations and
// reporting the remainder as a count.
func auditStateHuman(state AuditState, cap int) string {
	shown := min(len(state.Violations), cap)
	var human strings.Builder
	if state.LastResult == "" && state.LastAt == "" {
		human.WriteString("last audit: never\n")
	} else {
		fmt.Fprintf(&human, "last audit: %s", oneLine(state.LastResult))
		if state.LastAt != "" {
			fmt.Fprintf(&human, " at=%s", oneLine(state.LastAt))
		}
		fmt.Fprintf(&human, " master=%s\n", oneLine(state.LastMaster))
	}
	fmt.Fprintf(&human, "audit violations: total=%d", len(state.Violations))
	if omitted := len(state.Violations) - shown; omitted > 0 {
		fmt.Fprintf(&human, " omitted=%d", omitted)
	}
	for _, violation := range state.Violations[:shown] {
		fmt.Fprintf(&human, "\naudit violation: %s", oneLine(violation))
	}
	return human.String()
}

func runRecordsHuman(records []RunRecord, indent string) string {
	rows := make([]string, 0, len(records))
	for _, record := range records {
		rows = append(rows, fmt.Sprintf("%s%s agent=%s exit=%d duration=%.3fs",
			indent, oneLine(record.RunID), oneLine(record.Agent), record.Exit, record.Duration))
	}
	return strings.Join(rows, "\n")
}
