package main

import (
	"strings"
	"testing"
)

// TestSingletonLockExcludesASecondDaemon pins the oracle for #42: a manual
// `forest once` must not run while the daemon holds the singleton lock. The
// acquisition is non-blocking and fails with a named lock error, so the
// holder is undisturbed and the runner exits non-zero.
func TestSingletonLockExcludesASecondDaemon(t *testing.T) {
	repoDir := t.TempDir()

	holder, err := acquireSingletonLock(repoDir)
	if err != nil {
		t.Fatalf("first acquisition: %v", err)
	}

	blocked, err := acquireSingletonLock(repoDir)
	if err == nil {
		blocked.Close()
		t.Fatal("second acquisition while the lock is held must fail non-blocking")
	}
	if !strings.Contains(err.Error(), "daemon.lock") {
		t.Fatalf("error %q must name the lock", err)
	}

	// The holder is undisturbed; closing its fd releases the lock.
	if err := holder.Close(); err != nil {
		t.Fatalf("holder close: %v", err)
	}
	released, err := acquireSingletonLock(repoDir)
	if err != nil {
		t.Fatalf("acquisition after release: %v", err)
	}
	released.Close()
}
