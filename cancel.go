package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	runCancelledExit  = 130
	runCancelledError = "run cancelled by operator"
)

var errRunCancelled = errors.New(runCancelledError)

type cancelRunPayload struct {
	RunID string `json:"run_id"`
	State string `json:"state"`
}

// runCancellationMarkerPath names the durable marker a successful cancel leaves
// until the Runner records the cancelled outcome in the Ledger. The Runner
// owns the run log; the marker is its companion for the cancellation cause.
func runCancellationMarkerPath(root, runID string) string {
	return forestPath(root, "runs", runID+".cancel")
}

func hasRunCancellationMarker(root, runID string) bool {
	info, err := os.Stat(runCancellationMarkerPath(root, runID))
	return err == nil && info.Mode().IsRegular()
}

func writeRunCancellationMarker(root, runID string) error {
	path := runCancellationMarkerPath(root, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(runCancelledError+"\n"), 0o644)
}

// canonicalRoot resolves the checkout identity for comparing a live process's
// FOREST_ROOT marker with the checkout the operator asked to act on. Symlinks
// are followed so two spellings of one checkout do not read as two.
func canonicalRoot(root string) (string, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return absolute, nil
	}
	return resolved, nil
}

// readProcessEnvironment reads one process's environment from /proc. The file
// is a NUL-separated sequence of key=value entries and can change while being
// read, so the reader is best-effort and only used to identify a live Run.
func readProcessEnvironment(pid int) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, field := range strings.Split(string(data), "\x00") {
		if field == "" {
			continue
		}
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		values[key] = value
	}
	return values, nil
}

// findLiveRunProcessGroups returns the distinct process group IDs of processes
// carrying this checkout's FOREST_ROOT and the requested FOREST_RUN_ID marker.
// A process without the checkout marker is never reported, so a cancel in one
// checkout cannot target a live Run of another checkout.
func findLiveRunProcessGroups(root, runID string) ([]int, error) {
	rootCanonical, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	groups := make([]int, 0)
	seen := make(map[int]bool)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 1 {
			continue
		}
		environment, err := readProcessEnvironment(pid)
		if err != nil || environment["FOREST_RUN_ID"] != runID {
			continue
		}
		processRoot, ok := environment["FOREST_ROOT"]
		if !ok {
			continue
		}
		processRootCanonical, err := canonicalRoot(processRoot)
		if err != nil || processRootCanonical != rootCanonical {
			continue
		}
		pgid, err := syscall.Getpgid(pid)
		if err != nil || pgid <= 1 {
			continue
		}
		if !seen[pgid] {
			seen[pgid] = true
			groups = append(groups, pgid)
		}
	}
	return groups, nil
}

// runRunCancel stops one live Run's process group and records the cancellation
// through the Runner. A Run already in the Ledger is already finished; a Run
// that was already cancelled but has not reached the Ledger yet carries the
// cancellation marker, so the second cancel still reports already_finished.
func runRunCancel(rest []string, flags cliFlags) cliOutcome {
	runID := rest[0]
	if strings.TrimSpace(runID) == "" {
		return failure(exitInvalidArg, "run id must not be empty")
	}
	if _, found, err := FindRun(flags.root, runID); err != nil {
		return failure(exitError, "%s", err)
	} else if found {
		return cancelRunOutcome(runID, "already_finished")
	}

	groups, err := findLiveRunProcessGroups(flags.root, runID)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	if len(groups) == 0 {
		if hasRunCancellationMarker(flags.root, runID) {
			return cancelRunOutcome(runID, "already_finished")
		}
		return failure(exitNotFound, "run %q not found; see forest run list", runID)
	}

	if err := writeRunCancellationMarker(flags.root, runID); err != nil {
		return failure(exitError, "%s", err)
	}
	var stopErr error
	for _, pgid := range groups {
		if err := stopResidualProcessGroup(pgid, processStopGrace); err != nil {
			stopErr = errors.Join(stopErr, err)
		}
	}
	if stopErr != nil {
		return failure(exitError, "%s", stopErr)
	}
	return cancelRunOutcome(runID, "cancelled")
}

func cancelRunOutcome(runID, state string) cliOutcome {
	return cliOutcome{
		Exit:  exitOK,
		Data:  cancelRunPayload{RunID: runID, State: state},
		Human: fmt.Sprintf("run %s %s", oneLine(runID), state),
	}
}
