package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// verdictNote records a review decision for one exact commit.
type verdictNote struct {
	Verdict  string `json:"verdict"`
	Notes    string `json:"notes"`
	Reviewer string `json:"reviewer"`
	Model    string `json:"model"`
	DefSHA   string `json:"def_sha"`
	RunID    string `json:"run_id"`
	Time     string `json:"time"`
}

// checkResult records one gate command and its observed result.
type checkResult struct {
	Name    string  `json:"name"`
	Code    int     `json:"code"`
	Seconds float64 `json:"seconds"`
	Output  string  `json:"output"`
}

// checksNote records gate results for one exact commit.
type checksNote struct {
	Status  string        `json:"status"`
	Results []checkResult `json:"results"`
	RunID   string        `json:"run_id"`
	Time    string        `json:"time"`
}

type attemptsNote struct {
	Count int `json:"count"`
}

const (
	verdictNotesRef = "forest/verdict"
	checksNotesRef  = "forest/checks"
	reportNotesRef  = "forest/report"
)

var (
	errNoteExists      = errors.New("note already exists")
	errNoteInvalid     = errors.New("durable note is invalid")
	errAttemptsInvalid = errors.New("durable attempt record is invalid")
)

func notesRef(ref string) string {
	return "refs/notes/" + ref
}

func durableNotesRef(ref string) string {
	return "refs/notes/forest/durable/" + strings.TrimPrefix(ref, "refs/notes/")
}

func withNotesLock(repoDir string, fn func() error) error {
	commonDir, err := gitOut(repoDir, "rev-parse", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("resolve git common directory: %w", err)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(repoDir, commonDir)
	}
	lock, err := os.OpenFile(filepath.Join(commonDir, "forest-notes.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open notes lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return fmt.Errorf("lock durable notes: %w", err)
	}
	defer func() {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}()
	return fn()
}

// fetchNotes reconciles every durable notes ref with the remote under the same
// lock used by note writers.
func fetchNotes(repoDir string) error {
	return withNotesLock(repoDir, func() error {
		for _, ref := range []string{verdictNotesRef, checksNotesRef, reportNotesRef} {
			if err := fetchNoteRef(repoDir, ref); err != nil {
				return err
			}
		}
		return nil
	})
}

func missingRemoteRef(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "couldn't find remote ref") ||
		strings.Contains(msg, "could not find remote ref") ||
		strings.Contains(msg, "remote ref does not exist")
}
func readVerdict(repoDir, sha string) (verdictNote, bool, error) {
	body, ok, err := readNote(repoDir, verdictNotesRef, sha)
	if err != nil || !ok {
		return verdictNote{}, ok, err
	}
	var v verdictNote
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return verdictNote{}, false, fmt.Errorf("%w: decode verdict note: %v", errNoteInvalid, err)
	}
	if v.Verdict != "approve" && v.Verdict != "changes" {
		return verdictNote{}, false, fmt.Errorf("%w: Verdict note has an invalid decision", errNoteInvalid)
	}
	if strings.TrimSpace(v.Reviewer) == "" || strings.TrimSpace(v.Model) == "" || !validHex(v.DefSHA, 8) {
		return verdictNote{}, false, fmt.Errorf("%w: Verdict note has invalid attribution", errNoteInvalid)
	}
	return v, true, nil
}

func writeVerdict(repoDir, sha string, v verdictNote, id CommitIdentity) error {
	v.Time = time.Now().UTC().Format(time.RFC3339)
	return writeNote(repoDir, verdictNotesRef, sha, v, id)
}
func readChecks(repoDir, sha string) (checksNote, bool, error) {
	body, ok, err := readNote(repoDir, checksNotesRef, sha)
	if err != nil || !ok {
		return checksNote{}, ok, err
	}
	var c checksNote
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return checksNote{}, false, fmt.Errorf("%w: decode checks note: %v", errNoteInvalid, err)
	}
	if c.Status != "pass" && c.Status != "fail" {
		return checksNote{}, false, fmt.Errorf("%w: Checks note has an invalid status", errNoteInvalid)
	}
	if c.Status == "pass" {
		for _, result := range c.Results {
			if result.Code != 0 {
				return checksNote{}, false, fmt.Errorf(
					"%w: passing Checks note contains a failed result", errNoteInvalid)
			}
		}
	}
	return c, true, nil
}

func writeChecks(repoDir, sha string, c checksNote, id CommitIdentity) error {
	c.Time = time.Now().UTC().Format(time.RFC3339)
	return writeNote(repoDir, checksNotesRef, sha, c, id)
}

// readReport returns the durable Builder report for one exact commit, or ok
// false when the Revision carries none. The note stores the same fields the
// Gate validated in report.json, so a Verifier's prompt can carry the Builder's
// own account of what it chose and what it could not do — the one thing the
// body plus diff alone never supply.
func readReport(repoDir, sha string) (report, bool, error) {
	body, ok, err := readNote(repoDir, reportNotesRef, sha)
	if err != nil || !ok {
		return report{}, ok, err
	}
	var r report
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		return report{}, false, fmt.Errorf("%w: decode report note: %v", errNoteInvalid, err)
	}
	if strings.TrimSpace(r.Summary) == "" {
		return report{}, false, fmt.Errorf("%w: report note has an empty summary", errNoteInvalid)
	}
	return r, true, nil
}

func writeReport(repoDir, sha string, r report, id CommitIdentity) error {
	return writeNote(repoDir, reportNotesRef, sha, r, id)
}

func readNote(repoDir, ref, sha string) (body string, ok bool, err error) {
	err = withNotesLock(repoDir, func() error {
		// Readers use only the remote-confirmed snapshot. A local notes update
		// from an interrupted writer can never enter this ref.
		body, ok, err = readNoteUnlocked(repoDir, durableNotesRef(ref), sha)
		return err
	})
	return body, ok, err
}

func readNoteUnlocked(repoDir, ref, sha string) (string, bool, error) {
	cmd := exec.Command("git", "-C", repoDir, "notes", "--ref="+ref, "show", sha)
	out, err := runCombinedOutput(cmd)
	if err != nil {
		if noNote(err, out) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("git notes show: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), true, nil
}

func noNote(err error, output []byte) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(string(output))
	return strings.Contains(msg, "no note found") || strings.Contains(msg, "cannot read note")
}

func writeNote(repoDir, ref, sha string, value any, id CommitIdentity) error {
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s note: %w", ref, err)
	}
	// A note is pushed to the remote, so its stored text is outbound: a check
	// that printed an environment variable, or a verdict note that quoted one,
	// must be scrubbed before it becomes a durable remote fact.
	noteText := redactSecretShaped(string(body))
	return withNotesLock(repoDir, func() error {
		if err := fetchNoteRef(repoDir, ref); err != nil {
			return fmt.Errorf("reconcile %s note before write: %w", ref, err)
		}
		var pushErr error
		for attempt := range 3 {
			if err := gitAsIdentity(repoDir, id, "notes", "--ref="+ref, "add", "-m", noteText, sha); err != nil {
				if noteAlreadyExists(err) {
					return settleNoteWrite(repoDir, ref, sha, id, err)
				}
				return fmt.Errorf("write %s note: %w", ref, err)
			}
			noteHead, err := gitOut(repoDir, "rev-parse", notesRef(ref))
			if err != nil {
				return fmt.Errorf("resolve %s note update: %w", ref, err)
			}
			pushErr = git(repoDir, "push", "--no-verify", "origin", noteHead+":"+notesRef(ref))
			if pushErr == nil {
				if err := git(repoDir, "update-ref", durableNotesRef(ref), noteHead); err != nil {
					return fmt.Errorf("record durable %s note snapshot: %w", ref, err)
				}
				return nil
			}
			if attempt == 2 {
				if err := gitAsIdentity(repoDir, id, "notes", "--ref="+notesRef(ref), "remove", sha); err != nil {
					return fmt.Errorf("push %s note: %v; remove rejected local note: %w", ref, pushErr, err)
				}
				break
			}
			if err := fetchNoteRef(repoDir, ref); err != nil {
				return fmt.Errorf("fetch %s note after push rejection: %w", ref, err)
			}
		}
		return fmt.Errorf("push %s note after three attempts: %w", ref, pushErr)
	})
}

// settleNoteWrite decides what an existing local note means after a failed
// write. A note the remote already holds is a genuine durable winner. A note
// that exists only locally is untrusted outbound data from an interrupted or
// older writer, so the caller removes it rather than publishing it.
func settleNoteWrite(repoDir, ref, sha string, id CommitIdentity, cause error) error {
	durable, err := remoteHasNote(repoDir, ref, sha)
	if err != nil {
		return fmt.Errorf("settle %s note: %w", ref, err)
	}
	if !durable {
		if derr := gitAsIdentity(repoDir, id, "notes", "--ref="+notesRef(ref), "remove", sha); derr != nil {
			return fmt.Errorf("settle %s note: remove untrusted local note: %w", ref, derr)
		}
		return fmt.Errorf("note is local only; removed before remote publication: %v", cause)
	}
	if err := fetchNoteRef(repoDir, ref); err != nil {
		return fmt.Errorf("settle %s note: sync remote: %w", ref, err)
	}
	return fmt.Errorf("%w: %v", errNoteExists, cause)
}

// remoteHasNote reports whether the remote notes ref holds a note for sha. It
// inspects the remote through a scratch ref so a stale local note can never be
// mistaken for a durable remote fact.
func remoteHasNote(repoDir, ref, sha string) (bool, error) {
	probe := notesRef(ref) + "-probe"
	defer func() {
		_ = git(repoDir, "update-ref", "-d", probe)
	}()
	if err := git(repoDir, "fetch", "origin", "+"+notesRef(ref)+":"+probe); err != nil {
		if missingRemoteRef(err) {
			return false, nil
		}
		return false, fmt.Errorf("fetch %s note to probe remote: %w", ref, err)
	}
	_, ok, err := readNoteUnlocked(repoDir, probe, sha)
	return ok, err
}

func noteAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "found existing notes for object") ||
		strings.Contains(msg, "a note already exists for object")
}

func fetchNoteRef(repoDir, ref string) error {
	spec := "+" + notesRef(ref) + ":" + notesRef(ref)
	if err := git(repoDir, "fetch", "origin", spec); err != nil {
		if !missingRemoteRef(err) {
			return err
		}
		// The remote holds no such notes ref. Delete both the mutable writer
		// ref and the durable reader snapshot so local residue cannot survive.
		var errs []error
		for _, local := range []string{notesRef(ref), durableNotesRef(ref)} {
			if err := git(repoDir, "update-ref", "-d", local); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}
	head, err := gitOut(repoDir, "rev-parse", notesRef(ref))
	if err != nil {
		return err
	}
	return git(repoDir, "update-ref", durableNotesRef(ref), head)
}

func decodeAttempts(body string) (int, error) {
	if strings.TrimSpace(body) == "" {
		return 0, fmt.Errorf("%w: empty attempt record", errAttemptsInvalid)
	}
	var note attemptsNote
	if err := json.Unmarshal([]byte(body), &note); err != nil {
		return 0, fmt.Errorf("%w: decode attempt record: %v", errAttemptsInvalid, err)
	}
	if note.Count < 1 {
		return 0, fmt.Errorf("%w: invalid attempt count", errAttemptsInvalid)
	}
	return note.Count, nil
}

func readAttempts(repoDir, key string) (int, error) {
	sha, body, err := getBlobRef(repoDir, "refs/forest/attempt/"+key)
	if err != nil {
		return 0, fmt.Errorf("%w: read attempt record: %v", errFlowRetryable, err)
	}
	if sha == "" {
		return 0, nil
	}
	return decodeAttempts(body)
}

func bumpAttempts(repoDir, key string) (int, error) {
	ref := "refs/forest/attempt/" + key
	var casErr error
	for range 5 {
		sha, body, err := getBlobRef(repoDir, ref)
		if err != nil {
			return 0, fmt.Errorf("%w: read attempt record: %v", errFlowRetryable, err)
		}
		count := 0
		if sha != "" {
			count, err = decodeAttempts(body)
			if err != nil {
				return 0, err
			}
		}
		count++
		payload, err := json.Marshal(attemptsNote{Count: count})
		if err != nil {
			return 0, fmt.Errorf("encode attempt record: %w", err)
		}
		if err := putBlobRef(repoDir, ref, string(payload), sha); err == nil {
			return count, nil
		} else if !errors.Is(err, errRefMoved) {
			return 0, fmt.Errorf("%w: write attempt record: %v", errFlowRetryable, err)
		} else {
			casErr = err
		}
	}
	return 0, fmt.Errorf("%w: bump attempts after five attempts: %v", errFlowRetryable, casErr)
}

// dropAttempts removes a subject's attempt record. A retired subject must not
// leave one behind: the old claim scheme made items permanently unworkable
// exactly this way, by leaving a ref nobody would ever read again.
func dropAttempts(repoDir, key string) error {
	ref := "refs/forest/attempt/" + key
	sha, _, err := getBlobRef(repoDir, ref)
	if err != nil {
		return fmt.Errorf("%w: read attempt record: %v", errFlowRetryable, err)
	}
	if sha == "" {
		return nil
	}
	if err := deleteRef(repoDir, ref, sha); err != nil {
		return fmt.Errorf("%w: delete attempt record: %v", errFlowRetryable, err)
	}
	return nil
}
func effectAttemptKey(kind, subject, revision string) string {
	return "effect-" + blobSHA(subject) + "-" + blobSHA(kind+"\x00"+revision)
}
func listEffectRefs(repoDir, subject string) ([]string, error) {
	prefix := "refs/forest/attempt/effect-" + blobSHA(subject) + "-"
	out, err := gitCommand(repoDir, "ls-remote", "origin", prefix+"*")
	if err != nil {
		return nil, fmt.Errorf("%w: list Effect claims: %v", errFlowRetryable, err)
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || !validHex(fields[0], 20) ||
			!strings.HasPrefix(fields[1], prefix) ||
			!validHex(strings.TrimPrefix(fields[1], prefix), 20) {
			return nil, fmt.Errorf("%w: invalid Effect claim ref listing", errAttemptsInvalid)
		}
		refs = append(refs, fields[1])
	}
	return refs, nil
}

func claimEffect(repoDir, kind, subject, revision string) error {
	key := effectAttemptKey(kind, subject, revision)
	attempts, err := readAttempts(repoDir, key)
	if err != nil {
		return err
	}
	if attempts != 0 {
		return fmt.Errorf("%w: prior %s attempt for %q at Revision %s has no visible response",
			errHostMergeUnavailable, kind, subject, revision)
	}
	attempts, err = bumpAttempts(repoDir, key)
	if err != nil {
		return fmt.Errorf("%w: persist %s attempt for %q: %v",
			errHostMergePending, kind, subject, err)
	}
	if attempts != 1 {
		return fmt.Errorf("%w: concurrent %s attempt already owns %q at Revision %s",
			errHostMergeUnavailable, kind, subject, revision)
	}
	return nil
}
