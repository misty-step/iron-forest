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
)

var errRefMoved = errors.New("ref moved")

func encodeRefComponent(value string) string {
	return hex.EncodeToString([]byte(value))
}

func gitCommand(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoDir}, args...)...)
	out, err := runCombinedOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
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

func writeBlob(repoDir, content string) (string, error) {
	cmd := exec.Command("git", "-C", repoDir, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(content)
	out, err := runCombinedOutput(cmd)
	if err != nil {
		return "", fmt.Errorf("git hash-object: %w: %s", err, strings.TrimSpace(string(out)))
	}
	objectSHA := strings.TrimSpace(string(out))
	if objectSHA == "" {
		return "", errors.New("git hash-object returned no object id")
	}
	return objectSHA, nil
}

func putBlobRef(repoDir, ref, content, expectSHA string) error {
	objectSHA, err := writeBlob(repoDir, content)
	if err != nil {
		return err
	}
	cas := fmt.Sprintf("--force-with-lease=%s:%s", ref, expectSHA)
	_, err = gitCommand(repoDir, "push", "--no-verify", cas, "origin", objectSHA+":"+ref)
	return refWriteError(err)
}

func deleteRef(repoDir, ref, expectSHA string) error {
	cas := fmt.Sprintf("--force-with-lease=%s:%s", ref, expectSHA)
	args := []string{"push"}
	if strings.HasPrefix(ref, "refs/forest/") {
		args = append(args, "--no-verify")
	}
	args = append(args, cas, "origin", ":"+ref)
	_, err := gitCommand(repoDir, args...)
	return refWriteError(err)
}

func getBlobRef(repoDir, ref string) (sha, content string, err error) {
	for range 3 {
		out, err := gitCommand(repoDir, "ls-remote", "origin", ref)
		if err != nil {
			return "", "", err
		}
		fields := strings.Fields(out)
		if len(fields) == 0 {
			return "", "", nil
		}
		sha = fields[0]
		// Fetch the named object without writing FETCH_HEAD or a shared local
		// ref. Concurrent Flow readers therefore cannot contend on one ref lock.
		if _, err := gitCommand(repoDir, "fetch", "--no-write-fetch-head", "origin", ref); err != nil {
			return "", "", err
		}
		current, err := gitCommand(repoDir, "ls-remote", "origin", ref)
		if err != nil {
			return "", "", err
		}
		currentFields := strings.Fields(current)
		if len(currentFields) == 0 || currentFields[0] != sha {
			continue
		}
		content, err = gitCommand(repoDir, "cat-file", "-p", sha)
		if err != nil {
			return "", "", err
		}
		return sha, content, nil
	}
	return "", "", fmt.Errorf("read ref %s: remote changed during three attempts", ref)
}

func blobSHA(content string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, "blob "+strconv.Itoa(len([]byte(content)))+"\x00")
	_, _ = io.WriteString(h, content)
	return hex.EncodeToString(h.Sum(nil))
}
