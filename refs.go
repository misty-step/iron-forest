package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

var errRefMoved = errors.New("ref moved")

func encodeRefComponent(value string) string {
	return hex.EncodeToString([]byte(value))
}
func validHex(value string, bytes int) bool {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != bytes {
		return false
	}
	for _, b := range decoded {
		if b != 0 {
			return true
		}
	}
	return false
}

func hostGitArgs(repoDir string, args ...string) []string {
	commandArgs := make([]string, 0, len(args)+4)
	commandArgs = append(commandArgs, "-c", "core.hooksPath=/dev/null")
	if repoDir != "" {
		commandArgs = append(commandArgs, "-C", repoDir)
	}
	return append(commandArgs, args...)
}

func gitCommand(repoDir string, args ...string) (string, error) {
	cmd := exec.Command("git", hostGitArgs(repoDir, args...)...)
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
	cmd := exec.Command("git", hostGitArgs(repoDir, "hash-object", "-w", "--stdin")...)
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

// deleteRefsAtomically removes one required ref and any present optional refs
// in one compare-and-delete transaction. A lost response is reconciled by
// checking that every selected ref is absent.
func deleteRefsAtomically(repoDir, requiredRef, requiredSHA string, optionalRefs ...string) error {
	leases := map[string]string{requiredRef: requiredSHA}
	for _, ref := range optionalRefs {
		sha, _, err := getBlobRef(repoDir, ref)
		if err != nil {
			return err
		}
		if sha != "" {
			leases[ref] = sha
		}
	}
	current, _, err := getBlobRef(repoDir, requiredRef)
	if err != nil {
		return err
	}
	if current != requiredSHA {
		return fmt.Errorf("%w: ref %s is %s, want %s",
			errRefMoved, requiredRef, current, requiredSHA)
	}
	refs := make([]string, 0, len(leases))
	for ref := range leases {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	args := []string{"push", "--no-verify", "--atomic"}
	for _, ref := range refs {
		args = append(args, fmt.Sprintf("--force-with-lease=%s:%s", ref, leases[ref]))
	}
	args = append(args, "origin")
	for _, ref := range refs {
		args = append(args, ":"+ref)
	}
	if _, err := gitCommand(repoDir, args...); err == nil {
		return nil
	} else {
		for _, ref := range refs {
			sha, _, readErr := getBlobRef(repoDir, ref)
			if readErr != nil || sha != "" {
				return refWriteError(err)
			}
		}
		return nil
	}
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
