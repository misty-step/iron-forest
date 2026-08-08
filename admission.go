package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// errAdmissionHeld is the named refusal returned when another participant owns
// a Subject. A busy admission spends no agent run and writes no Ledger row.
var errAdmissionHeld = errors.New("admission refused: another participant holds the subject")

// hostID identifies one Unix owner on one operating-system installation.
// Admission fails closed when Linux cannot provide its stable machine identity.
var hostID = localHostID()

func localHostID() string {
	body, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return ""
	}
	return hostIDFromMachineID(string(body), os.Getuid())
}

func hostIDFromMachineID(text string, uid int) string {
	machineText := strings.TrimSpace(text)
	machineID, err := hex.DecodeString(machineText)
	if err != nil || len(machineID) != 16 || machineText == strings.Repeat("0", 32) || uid < 0 {
		return ""
	}
	var identity [24]byte
	copy(identity[:16], machineID)
	binary.BigEndian.PutUint64(identity[16:], uint64(uid))
	digest := sha256.Sum256(identity[:])
	return hex.EncodeToString(digest[:])
}

type admissionClaim struct {
	Flow     string `json:"flow"`
	Host     string `json:"host"`
	Revision string `json:"revision"`
}

// canonicalAdmissionKey joins item and factory-branch forms for one item. A
// singleton Subject keeps its declared stable key.
func canonicalAdmissionKey(s Subject) string {
	if s.ID != "" && (s.Kind == "item" || s.Kind == "branch" || s.Kind == "retirement") {
		return "item-" + s.ID
	}
	return s.Key
}

func admissionRef(key string) string {
	return "refs/forest/claim/" + encodeRefComponent(key)
}

// admissionLockPath ignores TMPDIR so every process for one Unix owner uses
// the same lock for one canonical repository and Subject.
func admissionLockPath(repository, key string) string {
	repository = strings.ToLower(repository)
	digest := sha256.Sum256([]byte(repository + "\x00" + key))
	root := filepath.Join("/tmp", fmt.Sprintf("iron-forest-%d", os.Getuid()))
	return filepath.Join(root, "admission", hex.EncodeToString(digest[:])+".lock")
}

// claimAdmission takes the per-owner lock before reading the durable claim. A
// same-Host stale claim is replaced by CAS; a foreign-Host claim is refused.
func claimAdmission(repoDir, repository, flow string, s Subject) (func(), error) {
	if hostID == "" {
		return nil, errors.New("admission: cannot determine Host identity")
	}
	key := canonicalAdmissionKey(s)
	lockPath := admissionLockPath(repository, key)
	lock, err := holdLock(lockPath)
	if err != nil {
		return nil, err
	}

	sha, body, err := getBlobRef(repoDir, admissionRef(key))
	if err != nil {
		dropLock(lock)
		return nil, fmt.Errorf("admission %s: %w", key, err)
	}

	var revision [16]byte
	if _, err := rand.Read(revision[:]); err != nil {
		dropLock(lock)
		return nil, fmt.Errorf("admission %s: create Revision: %w", key, err)
	}
	claim := admissionClaim{Flow: flow, Host: hostID, Revision: hex.EncodeToString(revision[:])}
	payload, err := json.Marshal(claim)
	if err != nil {
		dropLock(lock)
		return nil, fmt.Errorf("admission %s: encode: %w", key, err)
	}

	if sha != "" {
		var holder admissionClaim
		if err := json.Unmarshal([]byte(body), &holder); err != nil {
			dropLock(lock)
			return nil, fmt.Errorf("admission %s: decode claim: %w", key, err)
		}
		if holder.Host != hostID {
			dropLock(lock)
			return nil, fmt.Errorf("%w: %s (%s) on host %q", errAdmissionHeld, key, holder.Flow, holder.Host)
		}
		// The stable lock was acquired before this read, so a same-Host claim is stale.
	}
	if err := putBlobRef(repoDir, admissionRef(key), string(payload), sha); err != nil {
		dropLock(lock)
		if errors.Is(err, errRefMoved) {
			current, _, readErr := getBlobRef(repoDir, admissionRef(key))
			if readErr == nil && current != sha {
				return nil, fmt.Errorf("%w: %s", errAdmissionHeld, key)
			}
		}
		return nil, fmt.Errorf("admission %s: %w", key, err)
	}

	claimSHA := blobSHA(string(payload))
	return func() { releaseAdmission(repoDir, key, claimSHA, lock) }, nil
}

// releaseAdmission deletes the exact claim acquired by this participant before
// unlocking. A changed ref proves ownership ended; other failures are reported.
func releaseAdmission(repoDir, key, claimSHA string, lock *os.File) {
	if err := deleteRef(repoDir, admissionRef(key), claimSHA); err != nil {
		ownershipEnded := false
		if errors.Is(err, errRefMoved) {
			sha, _, readErr := getBlobRef(repoDir, admissionRef(key))
			ownershipEnded = readErr == nil && sha != claimSHA
		}
		if !ownershipEnded {
			fmt.Fprintf(os.Stderr, "forest: release admission %s: %v\n", key, err)
		}
	}
	dropLock(lock)
}

func holdLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", errAdmissionHeld, path)
		}
		return nil, fmt.Errorf("lock admission %s: %w", path, err)
	}
	return f, nil
}

func dropLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
