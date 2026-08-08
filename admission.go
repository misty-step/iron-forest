package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// errAdmissionHeld is the named refusal a participant returns when another
// participant already holds a Subject's admission. It is not a failure of this
// run: nothing was built or decided, so a caller treats it as busy and moves on
// to a different Subject.
var errAdmissionHeld = errors.New("admission refused: another participant holds the subject")

// hostname identifies the participant's host in the admission claim. It is a
// package variable so a test can simulate a second host.
var hostname = localHostname()

func localHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown-host"
	}
	return h
}

// admissionClaim is the durable fact that records who holds one Subject's
// admission. It is a blob pushed to a ref on origin, so a fresh clone observes
// the claim (admission is a git fact, not a host-local lease). Host and Lock let
// any participant on the same host decide, by flocking the holder's lock file,
// whether the holder is still alive; a participant on a different host cannot
// reach that lock and refuses rather than risk two participants on one Subject.
type admissionClaim struct {
	Key   string `json:"key"`
	Flow  string `json:"flow"`
	Host  string `json:"host"`
	Lock  string `json:"lock"`
	RunID string `json:"run_id"`
	Time  string `json:"time"`
}

// admissionRef is the durable, compare-and-set git ref that carries a Subject's
// admission claim. It uses the same refs/forest/ prefix as the attempt and
// stalled records, so each Subject's claim is a fact any clone can read.
func admissionRef(key string) string { return "refs/forest/claim/" + key }

// admissionLockPath is the host-local lock file whose flock is the liveness
// fact for a holder. A participant holds its own path; a competitor on the same
// host flocks the same path to learn whether the holder is still alive.
func admissionLockPath(repoDir, key string) string {
	return filepath.Join(workspaceDir(repoDir), "admit", key+".lock")
}

// claimAdmission claims one Subject for this participant before any expensive
// work starts. It publishes the claim as a compare-and-set git fact on origin,
// then guards it with a host-local flock. On success it returns a release
// function the caller must invoke when the run is over, whether it succeeded or
// failed. It returns errAdmissionHeld (wrapped with context) when another
// participant already holds the Subject.
//
// Liveness is a durable fact, never a timeout: a holder is alive iff its lock
// file is still flocked by a live process. When a holder crashes, the operating
// system drops its flock, so any later participant on the same host observes the
// lock as free and reclaims the Subject without operator action. A participant on
// a different host cannot reach the holder's lock and refuses, so two checkouts
// never duplicate one Subject's expensive work.
func claimAdmission(repoDir, flow, runID string, s Subject) (func(), error) {
	key := s.Key
	lock := admissionLockPath(repoDir, key)
	ref := admissionRef(key)
	for range 5 {
		// Create and hold our own lock file before publishing the claim, so a
		// competitor that reads the claim always finds its LockPath already
		// held by a live process and never misreads us as dead.
		myLock, err := holdLock(lock)
		if err != nil {
			return nil, err
		}
		sha, body, err := getBlobRef(repoDir, ref)
		if err != nil {
			dropLock(myLock, lock)
			return nil, fmt.Errorf("admission %s: %w", key, err)
		}
		if sha == "" {
			payload, perr := json.Marshal(admissionClaim{
				Key: key, Flow: flow, Host: hostname, Lock: lock,
				RunID: runID, Time: nowRFC(),
			})
			if perr != nil {
				dropLock(myLock, lock)
				return nil, fmt.Errorf("admission %s: encode: %w", key, perr)
			}
			if err := putBlobRef(repoDir, ref, string(payload), ""); err == nil {
				return func() { releaseAdmission(repoDir, key, lock, myLock) }, nil
			}
			dropLock(myLock, lock)
			if !errors.Is(err, errRefMoved) {
				return nil, fmt.Errorf("admission %s: %w", key, err)
			}
			continue // a competitor won the race; re-read and re-decide
		}
		var holder admissionClaim
		if err := json.Unmarshal([]byte(body), &holder); err != nil {
			dropLock(myLock, lock)
			return nil, fmt.Errorf("admission %s: decode claim: %w", key, err)
		}
		if holder.Host != hostname {
			// A foreign host holds the Subject. Its liveness is not observable
			// from here, so refuse rather than allow two Participants on one
			// Subject; the owning host recovers its own crashed run.
			dropLock(myLock, lock)
			return nil, fmt.Errorf("%w: %s (%s) on host %q", errAdmissionHeld, key, holder.Flow, holder.Host)
		}
		if lockHeld(holder.Lock) {
			// Same host and the holder's lock is still held: the holder is alive.
			dropLock(myLock, lock)
			return nil, fmt.Errorf("%w: %s (%s) on host %q", errAdmissionHeld, key, holder.Flow, holder.Host)
		}
		// Same host and the holder's lock is free: the holder crashed. Its claim
		// is a fact about abandoned work, so reclaim it by compare-and-set onto
		// the observed sha.
		_ = os.Remove(holder.Lock)
		payload, perr := json.Marshal(admissionClaim{
			Key: key, Flow: flow, Host: hostname, Lock: lock,
			RunID: runID, Time: nowRFC(),
		})
		if perr != nil {
			dropLock(myLock, lock)
			return nil, fmt.Errorf("admission %s: encode: %w", key, perr)
		}
		if err := putBlobRef(repoDir, ref, string(payload), sha); err == nil {
			return func() { releaseAdmission(repoDir, key, lock, myLock) }, nil
		}
		dropLock(myLock, lock)
		if !errors.Is(err, errRefMoved) {
			return nil, fmt.Errorf("admission %s: %w", key, err)
		}
		// A competitor reclaimed in the interval; loop to re-decide.
	}
	return nil, fmt.Errorf("admission %s: could not claim after five attempts", key)
}

// releaseAdmission drops the caller's flock and removes the durable claim by
// compare-and-set on the ref it currently observes, so a later holder's claim is
// never erased by a stale release.
func releaseAdmission(repoDir, key, lock string, myLock *os.File) {
	dropLock(myLock, lock)
	sha, _, err := getBlobRef(repoDir, admissionRef(key))
	if err != nil || sha == "" {
		return
	}
	_ = deleteRef(repoDir, admissionRef(key), sha)
}

// holdLock creates dirs, opens path, and takes a non-blocking exclusive flock on
// it, returning the open file that keeps the lock until the holder is done.
func holdLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("admission: lock %s is held: %w", path, err)
	}
	return f, nil
}

// dropLock releases and removes a lock file.
func dropLock(f *os.File, path string) {
	if f != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	_ = os.Remove(path)
}

// lockHeld reports whether path is currently flocked by a live holder. A path
// that cannot be opened is treated as dead: there is no process guarding it.
func lockHeld(path string) bool {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil
}

// isAdmissionHeld reports whether err is the named admission refusal. The
// command surface uses it to name a Subject that a daemon is already working.
func isAdmissionHeld(err error) bool {
	return errors.Is(err, errAdmissionHeld)
}
