package main

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errLeaseHeld = errors.New("lease held")

type leaseHandle struct {
	Key string
	Ref string
	SHA string
}

type leaseHolder struct {
	Flow  string `json:"flow"`
	RunID string `json:"run_id"`
	Host  string `json:"host"`
	PID   int    `json:"pid"`
	Time  string `json:"time"`
}

type leaseRef struct {
	Ref string
	SHA string
}

// leaseStore is the small repository surface needed for compare-and-set leases.
type leaseStore interface {
	create(ref, content, expectSHA string) error
	read(ref string) (sha, content string, err error)
	delete(ref, expectSHA string) error
	list(prefix string) ([]leaseRef, error)
}

type gitLeaseStore struct {
	repoDir string
}

func (s gitLeaseStore) create(ref, content, expectSHA string) error {
	return putBlobRef(s.repoDir, ref, content, expectSHA)
}

func (s gitLeaseStore) read(ref string) (string, string, error) {
	return getBlobRef(s.repoDir, ref)
}

func (s gitLeaseStore) delete(ref, expectSHA string) error {
	return pushLeaseDelete(s.repoDir, ref, expectSHA)
}

func (s gitLeaseStore) list(prefix string) ([]leaseRef, error) {
	out, err := gitCommand(s.repoDir, "ls-remote", "origin", prefix)
	if err != nil {
		return nil, err
	}
	var refs []leaseRef
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		refs = append(refs, leaseRef{Ref: fields[1], SHA: fields[0]})
	}
	return refs, nil
}

func gitCommand(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func gitBlob(repoDir, content string) (string, error) {
	cmd := exec.Command("git", "-C", repoDir, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git hash-object: %w: %s", err, strings.TrimSpace(string(out)))
	}
	sha := strings.TrimSpace(string(out))
	if sha == "" {
		return "", errors.New("git hash-object returned no object id")
	}
	return sha, nil
}

func pushLeaseRef(repoDir, ref, objectSHA, expectSHA string) error {
	lease := fmt.Sprintf("--force-with-lease=%s:%s", ref, expectSHA)
	_, err := gitCommand(repoDir, "push", lease, "origin", objectSHA+":"+ref)
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "stale info") ||
		strings.Contains(text, "non-fast-forward") ||
		strings.Contains(text, "fetch first") ||
		strings.Contains(text, "cannot lock ref") ||
		strings.Contains(text, "already exists") {
		return fmt.Errorf("%w: %v", errLeaseHeld, err)
	}
	return err
}

func putBlobRef(repoDir, ref, content, expectSHA string) error {
	objectSHA, err := gitBlob(repoDir, content)
	if err != nil {
		return err
	}
	return pushLeaseRef(repoDir, ref, objectSHA, expectSHA)
}

func pushLeaseDelete(repoDir, ref, expectSHA string) error {
	lease := fmt.Sprintf("--force-with-lease=%s:%s", ref, expectSHA)
	_, err := gitCommand(repoDir, "push", lease, "origin", ":"+ref)
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "stale info") ||
		strings.Contains(text, "non-fast-forward") ||
		strings.Contains(text, "fetch first") ||
		strings.Contains(text, "cannot lock ref") ||
		strings.Contains(text, "already exists") {
		return fmt.Errorf("%w: %v", errLeaseHeld, err)
	}
	return err
}

func getBlobRef(repoDir, ref string) (sha, content string, err error) {
	out, err := gitCommand(repoDir, "ls-remote", "origin", ref)
	if err != nil {
		return "", "", err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", "", nil
	}
	sha = fields[0]
	if _, err := gitCommand(repoDir, "fetch", "origin", "+"+ref+":"+ref); err != nil {
		return "", "", err
	}
	content, err = gitCommand(repoDir, "cat-file", "-p", ref)
	if err != nil {
		return "", "", err
	}
	return sha, content, nil
}

func blobSHA(content string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, "blob "+strconv.Itoa(len([]byte(content)))+"\x00")
	_, _ = io.WriteString(h, content)
	return hex.EncodeToString(h.Sum(nil))
}

func acquireLease(repoDir string, cfg Config, key, flow, runID string) (leaseHandle, error) {
	return acquireLeaseFrom(gitLeaseStore{repoDir: repoDir}, cfg, key, flow, runID)
}

func acquireLeaseFrom(store leaseStore, cfg Config, key, flow, runID string) (leaseHandle, error) {
	ref := "refs/forest/lease/" + key
	host, err := os.Hostname()
	if err != nil {
		return leaseHandle{}, fmt.Errorf("hostname: %w", err)
	}
	holder, err := json.Marshal(leaseHolder{
		Flow: flow, RunID: runID, Host: host, PID: os.Getpid(),
		Time: time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return leaseHandle{}, fmt.Errorf("lease holder: %w", err)
	}
	content := string(holder)
	if err := store.create(ref, content, ""); err == nil {
		return leaseHandle{Key: key, Ref: ref, SHA: blobSHA(content)}, nil
	} else if !errors.Is(err, errLeaseHeld) {
		return leaseHandle{}, err
	}

	ttl := cfg.Lease.TTL()
	if ttl <= 0 {
		return leaseHandle{}, errLeaseHeld
	}
	oldSHA, oldContent, err := store.read(ref)
	if err != nil || oldSHA == "" {
		return leaseHandle{}, errLeaseHeld
	}
	var old leaseHolder
	if json.Unmarshal([]byte(oldContent), &old) != nil {
		return leaseHandle{}, errLeaseHeld
	}
	oldTime, err := time.Parse(time.RFC3339, old.Time)
	if err != nil || time.Since(oldTime) <= ttl {
		return leaseHandle{}, errLeaseHeld
	}
	if err := store.delete(ref, oldSHA); err != nil {
		if errors.Is(err, errLeaseHeld) {
			return leaseHandle{}, errLeaseHeld
		}
		return leaseHandle{}, err
	}
	if err := store.create(ref, content, ""); err != nil {
		if errors.Is(err, errLeaseHeld) {
			return leaseHandle{}, errLeaseHeld
		}
		return leaseHandle{}, err
	}
	return leaseHandle{Key: key, Ref: ref, SHA: blobSHA(content)}, nil
}

func releaseLease(repoDir string, h leaseHandle) {
	releaseLeaseFrom(gitLeaseStore{repoDir: repoDir}, h)
}

func releaseLeaseFrom(store leaseStore, h leaseHandle) {
	if err := store.delete(h.Ref, h.SHA); err != nil {
		fmt.Fprintf(os.Stderr, "forest: release lease %s: %v\n", h.Key, err)
	}
}

func migrateLegacyClaims(repoDir string, cfg Config) {
	store := gitLeaseStore{repoDir: repoDir}
	refs, err := store.list("refs/heads/forest/claim/*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "forest: legacy ref migration: %v\n", err)
		return
	}
	removed := 0
	for _, ref := range refs {
		if err := store.delete(ref.Ref, ref.SHA); err != nil {
			fmt.Fprintf(os.Stderr, "forest: legacy ref %s: %v\n", ref.Ref, err)
			continue
		}
		removed++
	}
	fmt.Fprintf(os.Stderr, "forest: migrated %d legacy refs\n", removed)
}

type leaseBroker struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

func newLeaseBroker() *leaseBroker {
	return &leaseBroker{keys: make(map[string]struct{})}
}

func (b *leaseBroker) acquire(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.keys[key]; ok {
		return false
	}
	b.keys[key] = struct{}{}
	return true
}

func (b *leaseBroker) release(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.keys, key)
}

func (b *leaseBroker) idle() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.keys) == 0
}

var processLeases = newLeaseBroker()
