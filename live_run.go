package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// liveRunRecord is the on-disk owner record for one in-flight Run. It is
// written by the Runner at dispatch and removed when the Run finishes, so a
// reader never has to infer identity from a log mtime or run-id encoding.
type liveRunRecord struct {
	RunID         string            `json:"run_id"`
	Agent         string            `json:"agent"`
	StartedAt     string            `json:"started_at"`
	RequestID     string            `json:"request_id,omitempty"`
	Work          *WorkReference    `json:"work,omitempty"`
	DefinitionSHA string            `json:"definition_sha,omitempty"`
	ExtensionSHA  map[string]string `json:"extension_sha,omitempty"`
}

// LiveRunView is the read-surface answer for one live Run. StartedAt is the
// same UTC/RFC3339 timestamp the Runner recorded at dispatch; Elapsed is
// derived from that recorded timestamp, not from filesystem metadata.
type LiveRunView struct {
	RunID     string         `json:"run_id"`
	Agent     string         `json:"agent"`
	StartedAt string         `json:"started_at"`
	Elapsed   string         `json:"elapsed"`
	Cancel    string         `json:"cancel"`
	RequestID string         `json:"request_id,omitempty"`
	Work      *WorkReference `json:"work,omitempty"`
}

// liveRunPath names the per-agent live Run record. One file per agent is safe
// because the Scheduler permits at most one Run per agent at a time.
func liveRunPath(root, agent string) string {
	return forestPath(root, "runs", "live-"+agentSlug(agent)+".json")
}

// agentSlug keeps an agent name safe inside a file name. It is the same
// sanitizer newRunID uses, so the on-disk identity never diverges from the
// Run identity.
func agentSlug(agent string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, agent)
}

// writeLiveRun atomically replaces one agent's live Run record. The temporary
// file lives next to the target so the rename is on one filesystem.
func writeLiveRun(path string, record liveRunRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "live-*.json.tmp")
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncLedgerDirectory(filepath.Dir(path), defaultLedgerFileOps())
}

func liveRecord(record RunRecord) liveRunRecord {
	return liveRunRecord{
		RunID: record.RunID, Agent: record.Agent, StartedAt: record.Started,
		RequestID: record.RequestID, Work: record.Work,
		DefinitionSHA: record.DefinitionSHA, ExtensionSHA: record.ExtensionSHA,
	}
}

// readLiveRuns reads every live Run record, newest Run first. An absent runs
// directory means no Run has ever started.
func readLiveRuns(root string) ([]liveRunRecord, error) {
	dir := forestPath(root, "runs")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	records := make([]liveRunRecord, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "live-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read live run %s: %w", name, err)
		}
		var record liveRunRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("parse live run %s: %w", name, err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].RunID > records[j].RunID })
	return records, nil
}

// liveRunView renders one live Run record against a caller-supplied clock, so
// elapsed time is testable without sleeping.
func liveRunView(record liveRunRecord, now time.Time) LiveRunView {
	view := LiveRunView{RunID: record.RunID, Agent: record.Agent, StartedAt: record.StartedAt,
		RequestID: record.RequestID, Work: record.Work}
	if record.RunID != "" {
		view.Cancel = "forest run cancel " + record.RunID
	}
	if record.StartedAt != "" {
		if started, err := time.Parse(time.RFC3339Nano, record.StartedAt); err == nil {
			elapsed := now.Sub(started)
			if elapsed < 0 {
				elapsed = 0
			}
			view.Elapsed = elapsed.Round(time.Second).String()
		}
	}
	return view
}

// Called only by a new Kernel holding the instance lock. Orphaned subprocesses
// cannot be resumed safely: stop their verified Run groups, preserve attribution
// as interrupted evidence, and only then let startup remove reserved worktrees.
func recoverInterruptedRuns(root string) error {
	records, err := readLiveRuns(root)
	if err != nil {
		return err
	}
	for _, live := range records {
		if !isReservedRunID(live.RunID) || live.Agent == "" {
			return fmt.Errorf("invalid interrupted Run identity %q", live.RunID)
		}
		groups, err := findLiveRunProcessGroups(root, live.RunID)
		if err != nil {
			return err
		}
		for _, group := range groups {
			if err := stopResidualProcessGroup(group, processStopGrace); err != nil {
				return fmt.Errorf("stop interrupted Run %s: %w", live.RunID, err)
			}
		}
		if _, found, err := FindRun(root, live.RunID); err != nil {
			return err
		} else if !found {
			record := RunRecord{RunID: live.RunID, Agent: live.Agent, Started: live.StartedAt,
				RequestID: live.RequestID, Work: live.Work, DefinitionSHA: live.DefinitionSHA,
				ExtensionSHA: live.ExtensionSHA, Exit: 137, Error: "Run interrupted before completion"}
			if hasRunCancellationMarker(root, live.RunID) {
				record.Exit, record.Error = runCancelledExit, runCancelledError
			}
			// No completion time survived; never count downtime as execution.
			if err := AppendRun(root, record); err != nil {
				return err
			}
		}
		if err := os.Remove(liveRunPath(root, live.Agent)); err != nil {
			return err
		}
		_ = os.Remove(runCancellationMarkerPath(root, live.RunID))
	}
	return nil
}
