package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runRecord is one append-only ledger row for one flow decision.
type runRecord struct {
	Time          string `json:"time"`
	RunID         string `json:"run_id"`
	Flow          string `json:"flow"`
	Subject       string `json:"subject"`
	Revision      string `json:"revision"`
	ID            string `json:"issue"`
	Branch        string `json:"branch"`
	PRURL         string `json:"pr_url"`
	Status        string `json:"status"`
	TokensIn      int64  `json:"tokens_in"`
	TokOut        int64  `json:"tokens_out"`
	CacheRead     int64  `json:"cache_read"`
	CacheWrite    int64  `json:"cache_write"`
	Reasoning     int64  `json:"reasoning"`
	Agent         string `json:"agent"`
	Model         string `json:"model"`
	BaseSHA       string `json:"base_sha"`
	DefSHA        string `json:"def_sha"`
	ReviewVerdict string `json:"review,omitempty"`
	Error         string `json:"error,omitempty"`
}

// setTokens places every token class a flow reported onto a ledger record.
// Each measured class must reach the row shape a person reads; copying them all
// here keeps the ledger from discarding a class the harness observed.
func (r *runRecord) setTokens(o Outcome) {
	r.TokensIn = o.TokIn
	r.TokOut = o.TokOut
	r.CacheRead = o.CacheRead
	r.CacheWrite = o.CacheWrite
	r.Reasoning = o.Reasoning
}

// UnmarshalJSON reads one ledger row and tolerates both the legacy integer
// `issue` value and the opaque string id this build writes. The ledger is
// append-only, so rows written before the migration carry `"issue": 69` and
// must stay readable; a non-numeric id can only arrive as a string.
func (r *runRecord) UnmarshalJSON(data []byte) error {
	type runRecordAlias runRecord
	var aux struct {
		runRecordAlias
		Issue json.RawMessage `json:"issue"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = runRecord(aux.runRecordAlias)
	raw := bytes.TrimSpace(aux.Issue)
	switch {
	case len(raw) == 0, string(raw) == "null":
		// no issue field at all
	case len(raw) > 0 && raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		r.ID = s
	default:
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			return err
		}
		r.ID = strconv.Itoa(n)
	}
	return nil
}

// nowRFC returns the UTC timestamp used by ledger records.
func nowRFC() string { return time.Now().UTC().Format(time.RFC3339) }

// loadLedger reads every run from the append-only ledger at path. Unparseable
// lines are skipped and counted so one bad artifact never breaks the whole
// report. An empty ledger is a valid state and yields zero runs without error.
// This is the single loader every reader of the run ledger uses.
func loadLedger(path string) ([]runRecord, int, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var out []runRecord
	var invalid int
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r runRecord
		if err := json.Unmarshal(line, &r); err != nil {
			invalid++
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, fmt.Errorf("read ledger: %w", err)
	}
	return out, invalid, nil
}

// ledgerPath is the run ledger for a checkout.
func ledgerPath(repoDir string) string {
	return filepath.Join(workspaceDir(repoDir), "runs.jsonl")
}

// stalledRunLimit is how many times one flow may fail on one revision before it
// stops selecting it.
const stalledRunLimit = 3

// stalledRecord is the durable brake for one flow's subject. A revision change
// resets the count because it represents a new situation.
type stalledRecord struct {
	Revision string `json:"revision"`
	Count    int    `json:"count"`
}

// stalledRef returns the repository ref that survives checkout and host changes.
func stalledRef(flow, subject string) string {
	return "refs/forest/stalled/" + flow + "/" + subject
}

// stalledOn reports whether a flow reached the failure limit on this revision.
func stalledOn(repoDir, flow, subject, revision string) (bool, error) {
	if revision == "" {
		return false, nil
	}
	_, body, err := getBlobRef(repoDir, stalledRef(flow, subject))
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(body) == "" {
		return false, nil
	}
	var record stalledRecord
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		return false, fmt.Errorf("decode stalled record: %w", err)
	}
	return record.Revision == revision && record.Count >= stalledRunLimit, nil
}

// recordStalled increments the durable failure count with compare-and-swap.
func recordStalled(repoDir, flow, subject, revision string) error {
	if revision == "" {
		return errors.New("stalled: empty revision")
	}
	ref := stalledRef(flow, subject)
	var casErr error
	for range 5 {
		sha, body, err := getBlobRef(repoDir, ref)
		if err != nil {
			return err
		}
		record := stalledRecord{Revision: revision, Count: 1}
		if strings.TrimSpace(body) != "" {
			var previous stalledRecord
			if err := json.Unmarshal([]byte(body), &previous); err != nil {
				return fmt.Errorf("decode stalled record: %w", err)
			}
			if previous.Revision == revision {
				record.Count = previous.Count + 1
			}
		}
		payload, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("encode stalled record: %w", err)
		}
		if err := putBlobRef(repoDir, ref, string(payload), sha); err == nil {
			return nil
		} else if !errors.Is(err, errRefMoved) {
			return err
		} else {
			casErr = err
		}
	}
	return fmt.Errorf("record stalled after five attempts: %w", casErr)
}

// parkedRecord is the durable hold for a subject whose flow failed
// mechanically. Like the stalled brake it keys to one revision; unlike it, a
// park never spends the content budget that exists for failures the agent could
// plausibly fix. The Cause is the scrubbed error text of the last park, so the
// hold an operator reads stays visible.
type parkedRecord struct {
	Revision string `json:"revision"`
	Count    int    `json:"count"`
	Cause    string `json:"cause,omitempty"`
}

// parkedRef returns the repository ref that records a mechanical park.
func parkedRef(flow, subject string) string {
	return "refs/forest/parked/" + flow + "/" + subject
}

// parkedOn reports whether a flow reached the mechanical park limit on this
// revision. A park is keyed to the revision, so any movement — an item touching,
// a branch head advancing after a manual repair — releases the hold without
// deleting a ref or clearing a budget.
func parkedOn(repoDir, flow, subject, revision string) (bool, error) {
	if revision == "" {
		return false, nil
	}
	_, body, err := getBlobRef(repoDir, parkedRef(flow, subject))
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(body) == "" {
		return false, nil
	}
	var record parkedRecord
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		return false, fmt.Errorf("decode parked record: %w", err)
	}
	return record.Revision == revision && record.Count >= stalledRunLimit, nil
}

// parkFailure records one mechanical failure with its cause, compare-and-swap.
// The count is deliberately separate from the stalled brake: a mechanical
// failure — a host or provider outage, a worktree error, a rebase conflict, a
// publish race, a preflight — is retried on later passes but must never spend
// the budget reserved for content the agent could fix. A revision change resets
// the count because it represents a new situation.
func parkFailure(repoDir, flow, subject, revision string, cause error) error {
	if revision == "" {
		return errors.New("park: empty revision")
	}
	ref := parkedRef(flow, subject)
	var casErr error
	for range 5 {
		sha, body, err := getBlobRef(repoDir, ref)
		if err != nil {
			return err
		}
		record := parkedRecord{Revision: revision, Count: 1, Cause: redactSecretShaped(cause.Error())}
		if strings.TrimSpace(body) != "" {
			var previous parkedRecord
			if err := json.Unmarshal([]byte(body), &previous); err != nil {
				return fmt.Errorf("decode parked record: %w", err)
			}
			if previous.Revision == revision {
				record.Count = previous.Count + 1
			}
		}
		payload, err := json.Marshal(record)
		if err != nil {
			return fmt.Errorf("encode parked record: %w", err)
		}
		if err := putBlobRef(repoDir, ref, string(payload), sha); err == nil {
			return nil
		} else if !errors.Is(err, errRefMoved) {
			return err
		} else {
			casErr = err
		}
	}
	return fmt.Errorf("park failure after five attempts: %w", casErr)
}

// runCategory groups ledger statuses for operator summaries.
func runCategory(status string) string {
	switch status {
	case "built", "reviewed", "merged", "fixed":
		return "progress"
	case "skipped":
		return "other"
	default:
		if strings.HasSuffix(status, "_failed") {
			return "failed"
		}
		return "other"
	}
}

func appendRun(workspace string, r runRecord) error {
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(workspace, "runs.jsonl"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	return enc.Encode(r)
}
