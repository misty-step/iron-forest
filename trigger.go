package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// This file owns persisted trigger state: how it is read, how it is published,
// how it is resolved against a live Kernel, and how it is rendered. Every reader
// goes through here so no two commands can disagree about a trigger.

// readTriggerHealth returns persisted trigger state. The second result is false
// when no state exists yet, which is the normal condition on a checkout where
// the Kernel has never run and is not a failure.
func readTriggerHealth(root string) (map[string]TriggerHealth, bool, error) {
	data, err := os.ReadFile(forestPath(root, "triggers.json"))
	if os.IsNotExist(err) {
		return map[string]TriggerHealth{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var decoded map[string]*TriggerHealth
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, true, fmt.Errorf("parse trigger state: %w", err)
	}
	if decoded == nil {
		return nil, true, fmt.Errorf("parse trigger state: expected object")
	}
	values := make(map[string]TriggerHealth, len(decoded))
	for agent, health := range decoded {
		if health == nil {
			return nil, true, fmt.Errorf("parse trigger state: entry %q is null", agent)
		}
		if health.Agent != agent {
			return nil, true, fmt.Errorf("parse trigger state: entry %q has agent %q", agent, health.Agent)
		}
		values[agent] = *health
	}
	return values, true, nil
}

// writeTriggerHealth publishes trigger state by atomic replace. The Scheduler and
// the CLI share this writer so the durability protocol is stated once.
func writeTriggerHealth(root string, health map[string]TriggerHealth) error {
	data, err := json.MarshalIndent(health, "", "  ")
	if err != nil {
		return err
	}
	path := forestPath(root, "triggers.json")
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
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
	return os.Rename(tmpName, path)
}

// clearPersistedAuditErrors clears every agent's persisted audit_error after a
// successful Audit. Trigger rows summarize current dispatch health, so a
// transport failure recorded by an earlier Run must not outlive a later Audit
// that completed. Audit history stays in audit.log and Run evidence.
func clearPersistedAuditErrors(root string) error {
	health, present, err := readTriggerHealth(root)
	if err != nil || !present {
		return err
	}
	changed := false
	for agent, value := range health {
		if value.AuditError == "" {
			continue
		}
		value.AuditError = ""
		health[agent] = value
		changed = true
	}
	if !changed {
		return nil
	}
	return writeTriggerHealth(root, health)
}

// TriggerView is one agent's trigger state resolved against Kernel liveness. It
// is the published shape for both `trigger` commands and `status`.
type TriggerView struct {
	Name              string `json:"name"`
	StateKnown        bool   `json:"state_known"`
	ConsecutiveErrors int    `json:"consecutive_errors"`
	LastCode          int    `json:"last_code"`
	Running           bool   `json:"running"`
	RunningKnown      bool   `json:"running_known"`
	Stale             bool   `json:"stale"`
	LastRun           string `json:"last_run,omitempty"`
	PollError         string `json:"poll_error,omitempty"`
	RunError          string `json:"run_error,omitempty"`
	AuditError        string `json:"audit_error,omitempty"`
}

// triggerState is trigger state resolved against Kernel liveness. Unreadable or
// mismatched state is reported in StateErr and rendered as unknown, never as a
// missing answer: a reader always learns the state of every configured agent.
type triggerState struct {
	Views        []TriggerView
	StateErr     error
	LockErr      error
	LockHeld     bool
	StatePresent bool
}

// resolveTriggerState is the only place that decides whether a trigger is
// running, stale, or unknown, so every renderer and command agrees. The returned
// error covers configuration only; state and lock problems are carried as data.
func resolveTriggerState(root string) (triggerState, error) {
	cfg, err := loadConfig(configPath(root))
	if err != nil {
		return triggerState{}, err
	}
	names := agentNames(cfg)
	state := triggerState{}
	health, present, healthErr := readTriggerHealth(root)
	state.StatePresent = present
	if healthErr != nil {
		state.StateErr = healthErr
		health = map[string]TriggerHealth{}
	}
	state.LockHeld, state.LockErr = kernelLockHeld(root)
	// State for an agent the configuration no longer declares never updates
	// again, so it is drift worth reporting. A configured agent with no row yet
	// is the normal condition after one agent runs, not a fault.
	if healthErr == nil {
		orphans := make([]string, 0)
		for agent := range health {
			if !slices.Contains(names, agent) {
				orphans = append(orphans, agent)
			}
		}
		if len(orphans) > 0 {
			slices.Sort(orphans)
			state.StateErr = fmt.Errorf("trigger state names agents the configuration does not: %s", strings.Join(orphans, " "))
		}
	}
	state.Views = make([]TriggerView, 0, len(names))
	for _, name := range names {
		// Knowledge is per agent: one agent's missing row never hides another's
		// recorded errors.
		value, known := health[name]
		view := TriggerView{Name: name, StateKnown: known, RunningKnown: state.LockErr == nil}
		if known {
			view.ConsecutiveErrors = value.ConsecutiveErrors
			view.LastCode = value.LastCode
			view.Running = state.LockErr == nil && state.LockHeld && value.Running
			view.Stale = value.Running && state.LockErr == nil && !state.LockHeld
			view.LastRun = value.LastRun
			view.PollError = value.PollError
			view.RunError = value.RunError
			view.AuditError = value.AuditError
		}
		state.Views = append(state.Views, view)
	}
	return state, nil
}

// triggerViewsHuman renders resolved trigger state. `status` and the `trigger`
// commands share this renderer.
func triggerViewsHuman(views []TriggerView) string {
	var human strings.Builder
	human.WriteString("triggers:")
	for _, view := range views {
		if !view.StateKnown {
			fmt.Fprintf(&human, "\n  %s state=unknown", view.Name)
			continue
		}
		fmt.Fprintf(&human, "\n  %s errors=%d code=%d running=", view.Name, view.ConsecutiveErrors, view.LastCode)
		if !view.RunningKnown {
			human.WriteString("unknown")
		} else {
			fmt.Fprintf(&human, "%t", view.Running)
		}
		// stale is always rendered so every row carries the same field set and an
		// absent key cannot read as false.
		fmt.Fprintf(&human, " stale=%t", view.Stale)
		for _, field := range []struct {
			label string
			value string
		}{{"poll_error", view.PollError}, {"run_error", view.RunError}, {"audit_error", view.AuditError}} {
			if field.value != "" {
				fmt.Fprintf(&human, " %s=%s", field.label, oneLine(field.value))
			}
		}
	}
	return human.String()
}

// liveRunsHuman reports the live Runs read from their owner records. Liveness
// is unknown only when the lock or the state file cannot be read; no records
// means no Run is live.
func liveRunsHuman(state triggerState, liveRuns []LiveRunView) string {
	if state.StateErr != nil {
		return "live runs: unknown"
	}
	for _, view := range state.Views {
		if !view.RunningKnown {
			return "live runs: unknown"
		}
	}
	if len(liveRuns) == 0 {
		return "live runs: none"
	}
	var human strings.Builder
	human.WriteString("live runs:")
	for _, run := range liveRuns {
		fmt.Fprintf(&human, "\n  run_id=%s agent=%s started_at=%s elapsed=%s cancel=%q",
			run.RunID, run.Agent, run.StartedAt, run.Elapsed, run.Cancel)
	}
	return human.String()
}
