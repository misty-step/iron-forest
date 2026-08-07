package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

var errRefMoved = errors.New("ref moved")

type refRecord struct {
	Ref string
	SHA string
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

func putRef(repoDir, ref, objectSHA, expectSHA string) error {
	cas := fmt.Sprintf("--force-with-lease=%s:%s", ref, expectSHA)
	_, err := gitCommand(repoDir, "push", cas, "origin", objectSHA+":"+ref)
	return refWriteError(err)
}

func refWriteError(err error) error {
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "stale info") ||
		strings.Contains(text, "non-fast-forward") ||
		strings.Contains(text, "fetch first") ||
		strings.Contains(text, "cannot lock ref") ||
		strings.Contains(text, "already exists") {
		return fmt.Errorf("%w: %v", errRefMoved, err)
	}
	return err
}

func putBlobRef(repoDir, ref, content, expectSHA string) error {
	objectSHA, err := gitBlob(repoDir, content)
	if err != nil {
		return err
	}
	return putRef(repoDir, ref, objectSHA, expectSHA)
}

func deleteRef(repoDir, ref, expectSHA string) error {
	cas := fmt.Sprintf("--force-with-lease=%s:%s", ref, expectSHA)
	_, err := gitCommand(repoDir, "push", cas, "origin", ":"+ref)
	return refWriteError(err)
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

func listRefs(repoDir, prefix string) ([]refRecord, error) {
	if !strings.Contains(prefix, "*") {
		prefix += "*"
	}
	out, err := gitCommand(repoDir, "ls-remote", "origin", prefix)
	if err != nil {
		return nil, err
	}
	var refs []refRecord
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			refs = append(refs, refRecord{Ref: fields[1], SHA: fields[0]})
		}
	}
	return refs, nil
}

func blobSHA(content string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, "blob "+strconv.Itoa(len([]byte(content)))+"\x00")
	_, _ = io.WriteString(h, content)
	return hex.EncodeToString(h.Sum(nil))
}

type subjectSet struct {
	mu   sync.Mutex
	keys map[string]struct{}
}

func newSubjectSet() *subjectSet {
	return &subjectSet{keys: make(map[string]struct{})}
}

func (b *subjectSet) claim(key string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.keys[key]; ok {
		return false
	}
	b.keys[key] = struct{}{}
	return true
}

func (b *subjectSet) release(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.keys, key)
}

func (b *subjectSet) idle() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.keys) == 0
}

var inFlight = newSubjectSet()
