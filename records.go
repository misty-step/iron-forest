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

// stalledRef keeps legacy valid refs stable and moves only opaque invalid keys
// into a disjoint encoded namespace.
func stalledRef(flow, subject string) string {
	if validRefSuffix(subject) {
		return "refs/forest/stalled/" + flow + "/" + subject
	}
	return "refs/forest/stalled-opaque/" + flow + "/" + encodeRefComponent(subject)
}

func validRefSuffix(value string) bool {
	if value == "" || value == "@" || strings.HasPrefix(value, "/") ||
		strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") ||
		strings.Contains(value, "..") || strings.Contains(value, "@{") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || strings.HasPrefix(component, ".") ||
			strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	for i := 0; i < len(value); i++ {
		if value[i] <= ' ' || value[i] == 0x7f || strings.ContainsRune(`~^:?*[\`, rune(value[i])) {
			return false
		}
	}
	return true
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
