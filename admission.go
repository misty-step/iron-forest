package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

var errAdmissionPaused = errors.New("instance admission is paused; use forest admission resume")

type AdmissionState struct {
	Paused    bool   `json:"paused"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type admissionView struct {
	AdmissionState
	ActiveRuns      []LiveRunView `json:"active_runs"`
	ActiveCount     int           `json:"active_count"`
	InterruptedRuns []LiveRunView `json:"interrupted_runs,omitempty"`
	Drained         bool          `json:"drained"`
}

// The gate serializes state changes and Run reservation, not Run execution.
// Each admitted Run holds a separate shared active lock until its final record.
// A drain holds the exclusive active lock only after all admitted Runs finish.
func admissionLock(root, name string, operation int) (*os.File, error) {
	path := forestPath(root, name+".lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := lockLedger(file, operation); err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return file, nil
}

func readAdmission(root string) (AdmissionState, error) {
	var state AdmissionState
	data, err := os.ReadFile(forestPath(root, "admission.json"))
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	var document struct {
		Paused    *bool  `json:"paused"`
		UpdatedAt string `json:"updated_at"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return state, fmt.Errorf("parse admission state: %w", err)
	}
	if document.Paused == nil {
		return state, errors.New("admission state must contain paused")
	}
	state.Paused, state.UpdatedAt = *document.Paused, document.UpdatedAt
	return state, nil
}

func setAdmission(root string, paused bool) error {
	gate, err := admissionLock(root, "admission", syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer gate.Close()
	state := AdmissionState{Paused: paused, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := forestPath(root, "admission.json")
	temp, err := os.CreateTemp(filepath.Dir(path), ".admission-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err := temp.Write(append(data, '\n')); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(err, temp.Close())
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	files := defaultLedgerFileOps()
	for _, directory := range []string{forestPath(root), filepath.Join(root, profileName), root} {
		if err := syncLedgerDirectory(directory, files); err != nil {
			return err
		}
	}
	return nil
}

func reserveRunAdmission(root string, record RunRecord) (*os.File, error) {
	gate, err := admissionLock(root, "admission", syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	state, err := readAdmission(root)
	if err != nil {
		return nil, err
	}
	if state.Paused {
		return nil, errAdmissionPaused
	}
	active, err := admissionLock(root, "active-runs", syscall.LOCK_SH|syscall.LOCK_NB)
	if err != nil {
		return nil, err
	}
	if err := writeLiveRun(liveRunPath(root, record.Agent), liveRecord(record)); err != nil {
		return nil, errors.Join(err, active.Close())
	}
	files := defaultLedgerFileOps()
	for _, directory := range []string{forestPath(root), filepath.Join(root, profileName), root} {
		if err := syncLedgerDirectory(directory, files); err != nil {
			return nil, errors.Join(err, active.Close())
		}
	}
	return active, nil
}

func admissionSnapshot(root string) (admissionView, error) {
	gate, err := admissionLock(root, "admission", syscall.LOCK_SH)
	if err != nil {
		return admissionView{}, err
	}
	defer gate.Close()
	state, err := readAdmission(root)
	if err != nil {
		return admissionView{}, err
	}
	active, lockErr := admissionLock(root, "active-runs", syscall.LOCK_EX|syscall.LOCK_NB)
	idle := lockErr == nil
	if idle {
		defer active.Close()
	} else if !errors.Is(lockErr, syscall.EWOULDBLOCK) && !errors.Is(lockErr, syscall.EAGAIN) {
		return admissionView{}, lockErr
	}
	records, err := readLiveRuns(root)
	if err != nil {
		return admissionView{}, err
	}
	view := admissionView{AdmissionState: state, ActiveRuns: []LiveRunView{}, Drained: state.Paused && idle}
	for _, record := range records {
		interrupted := idle
		if idle {
			groups, err := findLiveRunProcessGroups(root, record.RunID)
			if err != nil {
				return admissionView{}, err
			}
			interrupted = len(groups) == 0
		}
		if interrupted {
			view.InterruptedRuns = append(view.InterruptedRuns, liveRunView(record, time.Now()))
		} else {
			view.ActiveRuns = append(view.ActiveRuns, liveRunView(record, time.Now()))
			view.Drained = false
		}
	}
	view.ActiveCount = len(view.ActiveRuns)
	return view, nil
}

func runAdmissionShow(_ []string, flags cliFlags) cliOutcome {
	view, err := admissionSnapshot(flags.root)
	if err != nil {
		return failure(exitError, "%s", err)
	}
	return cliOutcome{Exit: exitOK, Data: view, Human: fmt.Sprintf("admission: paused=%t active=%d drained=%t", view.Paused, view.ActiveCount, view.Drained)}
}

func runAdmissionPause(_ []string, flags cliFlags) cliOutcome {
	if err := setAdmission(flags.root, true); err != nil {
		return failure(exitError, "%s", err)
	}
	return runAdmissionShow(nil, flags)
}

func runAdmissionResume(_ []string, flags cliFlags) cliOutcome {
	// Resume must not invalidate another operator's in-progress drain.
	drain, err := admissionLock(flags.root, "drain", syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return failure(exitConflict, "cannot resume while drain is active: %s", err)
	}
	defer drain.Close()
	if err := setAdmission(flags.root, false); err != nil {
		return failure(exitError, "%s", err)
	}
	return runAdmissionShow(nil, flags)
}

func drainAdmission(ctx context.Context, root string) error {
	drain, err := admissionLock(root, "drain", syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return err
	}
	defer drain.Close()
	if err := setAdmission(root, true); err != nil {
		return err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		active, err := admissionLock(root, "active-runs", syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			if err := active.Close(); err != nil {
				return err
			}
			view, err := admissionSnapshot(root)
			if err != nil {
				return err
			}
			if view.Drained {
				return nil
			}
		}
		if err != nil && !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runAdmissionDrain(_ []string, flags cliFlags) cliOutcome {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := drainAdmission(ctx, flags.root); err != nil {
		return failure(exitError, "admission remains paused; drain: %s", err)
	}
	return runAdmissionShow(nil, flags)
}
