package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const checkOutputTailBytes = 4000

// runChecks runs every declared check and records the result as a durable
// note. A check that exits non-zero sets the note to "fail"; a command that
// cannot start does too. A preflight failure — building the child environment,
// locating the toolchain, resolving FOREST_CHECK_PATH — means no declared check
// ran, so the returned note keeps an empty status and the caller must not write
// it as pass. The status is built from the observed result, never assumed.

func runChecks(cfg Config, wtDir, runID string, owningRepo ...string) (checksNote, error) {
	note := checksNote{
		RunID: runID,
		Time:  time.Now().UTC().Format(time.RFC3339),
	}
	repoDir := wtDir
	if len(owningRepo) > 1 {
		return note, fmt.Errorf("run checks: one owning checkout is allowed")
	}
	if len(owningRepo) == 1 {
		repoDir = owningRepo[0]
	}
	env, cleanup, err := checkEnvironment(repoDir, wtDir)
	if err != nil {
		return note, err
	}
	defer cleanup()
	sandbox, err := prepareChildSandbox(repoDir, wtDir, env)
	if err != nil {
		return note, err
	}

	var startErr error
	for _, check := range cfg.Checks {
		started := time.Now()
		ackRead, ackWrite, err := os.Pipe()
		if err != nil {
			return note, fmt.Errorf("prepare check %q launch acknowledgement: %w", check.Name, err)
		}
		cmd, err := sandbox.commandWithFiles(
			context.Background(),
			[]*os.File{ackWrite},
			"sh",
			"-c",
			`umask 077; exec /bin/sh -c "$1"`,
			"forest-check",
			check.Run,
		)
		if err != nil {
			_ = ackWrite.Close()
			_ = ackRead.Close()
			return note, err
		}
		output, runErr := runCombinedOutput(cmd)
		_ = ackWrite.Close()
		status, _ := io.ReadAll(ackRead)
		_ = ackRead.Close()
		if !sandboxCommandCompleted(status, runErr) {
			if runErr == nil {
				runErr = errors.New("Bubblewrap reported no child exit")
			}
			return note, fmt.Errorf("start check %q sandbox: %w: %s",
				check.Name, runErr, tailCheckOutput(output))
		}
		err = runErr
		result := checkResult{
			Name:    check.Name,
			Seconds: time.Since(started).Seconds(),
			Output:  tailCheckOutput(output),
		}
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				result.Code = exitErr.ExitCode()
				note.Status = "fail"
			} else {
				result.Code = -1
				note.Status = "fail"
				if startErr == nil {
					startErr = fmt.Errorf("start check %q: %w", check.Name, err)
				}
			}
		}
		note.Results = append(note.Results, result)
	}
	if startErr != nil {
		note.Status = "fail"
		return note, startErr
	}
	// Every check that set a failure already marked the note "fail"; an empty
	// status means all of them passed, so record the pass explicitly.
	if note.Status == "" {
		note.Status = "pass"
	}
	return note, nil
}

func sandboxCommandCompleted(status []byte, runErr error) bool {
	decoder := json.NewDecoder(bytes.NewReader(status))
	for {
		var event map[string]json.RawMessage
		if err := decoder.Decode(&event); err != nil {
			break
		}
		if _, ok := event["exit-code"]; ok {
			return true
		}
	}
	var exitErr *exec.ExitError
	return errors.As(runErr, &exitErr) && exitErr.ExitCode() == -1
}

// No per-check deadline: factory commands have their own bounds; killing one reports a failure this runner did not cause.
func tailCheckOutput(output []byte) string {
	if len(output) > checkOutputTailBytes {
		output = output[len(output)-checkOutputTailBytes:]
	}
	return string(output)
}

func checksSummary(c checksNote) string {
	if c.Status == "pass" {
		return "checks pass"
	}
	failed := make([]string, 0, len(c.Results))
	for _, result := range c.Results {
		if result.Code != 0 {
			failed = append(failed, fmt.Sprintf("%s(exit %d)", result.Name, result.Code))
		}
	}
	if len(failed) == 0 {
		return "checks fail"
	}
	return "checks fail: " + strings.Join(failed, ", ")
}
