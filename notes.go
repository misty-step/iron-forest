package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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
)

var (
	errNoteExists = errors.New("note already exists")
	// errLocalNote reports a note that exists only in the local checkout. A
	// note is a durable fact only when it also exists on the remote, so a
	// local-pending note must never be mistaken for a remote winner.
	errLocalNote = errors.New("note is local only; not durable on remote")
)

func notesRef(ref string) string {
	return "refs/notes/" + ref
}

// fetchNotes reconciles every durable notes ref with the remote. It iterates
// one ref at a time through fetchNoteRef so that a notes ref the remote does
// not have removes any stale local note instead of leaving it to shadow the
// absent remote fact.
func fetchNotes(repoDir string) error {
	for _, ref := range []string{verdictNotesRef, checksNotesRef} {
		if err := fetchNoteRef(repoDir, ref); err != nil {
			return err
		}
	}
	return nil
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
		return verdictNote{}, false, fmt.Errorf("decode verdict note: %w", err)
	}
	return v, true, nil
}

func writeVerdict(repoDir, sha string, v verdictNote) error {
	v.Time = time.Now().UTC().Format(time.RFC3339)
	return writeNote(repoDir, verdictNotesRef, sha, v)
}
func readChecks(repoDir, sha string) (checksNote, bool, error) {
	body, ok, err := readNote(repoDir, checksNotesRef, sha)
	if err != nil || !ok {
		return checksNote{}, ok, err
	}
	var c checksNote
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return checksNote{}, false, fmt.Errorf("decode checks note: %w", err)
	}
	return c, true, nil
}

func writeChecks(repoDir, sha string, c checksNote) error {
	c.Time = time.Now().UTC().Format(time.RFC3339)
	return writeNote(repoDir, checksNotesRef, sha, c)
}
func readNote(repoDir, ref, sha string) (string, bool, error) {
	cmd := exec.Command("git", "-C", repoDir, "notes", "--ref="+ref, "show", sha)
	out, err := cmd.CombinedOutput()
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

func writeNote(repoDir, ref, sha string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s note: %w", ref, err)
	}
	// A note is pushed to the remote, so its stored text is outbound: a check
	// that printed an environment variable, or a verdict note that quoted one,
	// must be scrubbed before it becomes a durable remote fact.
	noteText := redactSecretShaped(string(body))
	var pushErr error
	for attempt := range 3 {
		if err := git(repoDir, "notes", "--ref="+ref, "add", "-m", noteText, sha); err != nil {
			if noteAlreadyExists(err) {
				// The note already exists locally. It is only a durable fact if
				// the remote also holds it, so settle the disagreement before
				// reporting a win.
				return settleNoteWrite(repoDir, ref, sha, err)
			}
			return fmt.Errorf("write %s note: %w", ref, err)
		}
		pushErr = git(repoDir, "push", "origin", notesRef(ref))
		if pushErr == nil {
			return nil
		}
		if attempt == 2 {
			break
		}
		if err := fetchNoteRef(repoDir, ref); err != nil {
			return fmt.Errorf("fetch %s note after push rejection: %w", ref, err)
		}
	}
	return fmt.Errorf("push %s note after three attempts: %w", ref, pushErr)
}

// settleNoteWrite decides what an existing local note means after a failed
// write. A note the remote already holds is a genuine durable winner and is
// reported as errNoteExists; a note that exists only locally is a failed push
// that is retried. If the retry lands, the remote holds the older local note
// (not this caller's value), so it is still reported as errNoteExists for the
// caller to reread; if the retry cannot land, the local note is removed so it
// never silently satisfies a gate.
func settleNoteWrite(repoDir, ref, sha string, cause error) error {
	durable, err := remoteHasNote(repoDir, ref, sha)
	if err != nil {
		return fmt.Errorf("settle %s note: %w", ref, err)
	}
	if !durable {
		if perr := git(repoDir, "push", "origin", notesRef(ref)); perr == nil {
			// The retried push landed, but the value this caller computed was
			// never installed: git notes add aborted on the already-existing
			// local note. The remote now holds that older note, so report the
			// write-once win and let the caller reread the durable value rather
			// than trust its in-memory verdict/checks.
			return fmt.Errorf("%w: %v", errNoteExists, cause)
		} else if derr := git(repoDir, "notes", "--ref="+notesRef(ref), "remove", sha); derr != nil {
			return fmt.Errorf("settle %s note: push %v; remove %w", ref, perr, derr)
		}
		return fmt.Errorf("%w: %v", errLocalNote, cause)
	}
	// Sync the remote winner into the live notes ref so the caller's read
	// returns the durable note, then hand it the write-once signal.
	if err := git(repoDir, "fetch", "origin", "+"+notesRef(ref)+":"+notesRef(ref)); err != nil {
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
	_, ok, err := readNote(repoDir, probe, sha)
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
		// The remote holds no such notes ref, so any local note for it is stale:
		// delete the ref rather than let it shadow the absent remote fact.
		return git(repoDir, "update-ref", "-d", notesRef(ref))
	}
	return nil
}

func readAttempts(repoDir, key string) (int, error) {
	_, body, err := getBlobRef(repoDir, "refs/forest/attempt/"+key)
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(body) == "" {
		return 0, nil
	}
	var a attemptsNote
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		return 0, fmt.Errorf("decode attempt record: %w", err)
	}
	return a.Count, nil
}

func bumpAttempts(repoDir, key string) (int, error) {
	ref := "refs/forest/attempt/" + key
	var casErr error
	for range 5 {
		sha, body, err := getBlobRef(repoDir, ref)
		if err != nil {
			return 0, err
		}
		count := 0
		if strings.TrimSpace(body) != "" {
			var a attemptsNote
			if err := json.Unmarshal([]byte(body), &a); err != nil {
				return 0, fmt.Errorf("decode attempt record: %w", err)
			}
			count = a.Count
		}
		count++
		payload, err := json.Marshal(attemptsNote{Count: count})
		if err != nil {
			return 0, fmt.Errorf("encode attempt record: %w", err)
		}
		if err := putBlobRef(repoDir, ref, string(payload), sha); err == nil {
			return count, nil
		} else if !errors.Is(err, errRefMoved) {
			return 0, err
		} else {
			casErr = err
		}
	}
	return 0, fmt.Errorf("bump attempts after five attempts: %w", casErr)
}

// dropAttempts removes a subject's attempt record. A retired subject must not
// leave one behind: the old claim scheme made items permanently unworkable
// exactly this way, by leaving a ref nobody would ever read again.
func dropAttempts(repoDir, key string) error {
	ref := "refs/forest/attempt/" + key
	sha, _, err := getBlobRef(repoDir, ref)
	if err != nil {
		return err
	}
	if sha == "" {
		return nil
	}
	return git(repoDir, "push", "--force-with-lease="+ref+":"+sha, "origin", ":"+ref)
}
