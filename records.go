package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
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

const retirementRefPrefix = "refs/forest/retirement/"

type retirementRecord struct {
	Branch    string `json:"branch"`
	Revision  string `json:"revision"`
	ItemID    string `json:"item_id"`
	Transport string `json:"transport"`
	Strategy  string `json:"strategy"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Agent     string `json:"agent"`
	Model     string `json:"model"`
	DefSHA    string `json:"def_sha"`
}

type retirementFact struct {
	Ref    string
	SHA    string
	Record retirementRecord
}

func retirementRef(branch, revision string) string {
	return retirementRefPrefix + encodeRefComponent(branch) + "/" + encodeRefComponent(revision)
}

func retirementPayload(record retirementRecord) (string, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("encode retirement: %w", err)
	}
	return string(payload), nil
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

func validateRetirementRecord(ref string, record retirementRecord) error {
	if (record.Transport != "git" && record.Transport != "host") ||
		(record.Strategy != "squash" && record.Strategy != "ff") ||
		(record.State != "pending" && record.State != "landed") ||
		(record.Transport == "host" && record.Strategy != "squash") ||
		(record.Transport == "git" && record.State != "landed") {
		return fmt.Errorf("retirement %s has invalid transport/strategy/state %q/%q/%q",
			ref, record.Transport, record.Strategy, record.State)
	}
	name := strings.TrimPrefix(record.Branch, BranchPrefix)
	dash := strings.IndexByte(name, '-')
	if !strings.HasPrefix(record.Branch, BranchPrefix) || dash <= 0 ||
		record.ItemID == "" || encodeBranchID(record.ItemID) != name[:dash] {
		return fmt.Errorf("retirement %s has invalid branch/item identity %q/%q", ref, record.Branch, record.ItemID)
	}
	if !validHex(record.Revision, 20) {
		return fmt.Errorf("retirement %s has invalid Revision %q", ref, record.Revision)
	}
	if record.Agent == "" || record.Model == "" || !validHex(record.DefSHA, 8) {
		return fmt.Errorf("retirement %s has invalid agent attribution", ref)
	}
	if retirementRef(record.Branch, record.Revision) != ref {
		return fmt.Errorf("retirement %s content does not match its ref", ref)
	}
	return nil
}

func prepareRetirement(repoDir string, record retirementRecord) (retirementFact, error) {
	ref := retirementRef(record.Branch, record.Revision)
	if err := validateRetirementRecord(ref, record); err != nil {
		return retirementFact{}, err
	}
	payload, err := retirementPayload(record)
	if err != nil {
		return retirementFact{}, err
	}
	sha, err := writeBlob(repoDir, payload)
	if err != nil {
		return retirementFact{}, err
	}
	return retirementFact{
		Ref: retirementRef(record.Branch, record.Revision), SHA: sha, Record: record,
	}, nil
}

func readRetirement(repoDir, branch, revision string) (retirementFact, bool, error) {
	ref := retirementRef(branch, revision)
	sha, body, err := getBlobRef(repoDir, ref)
	if err != nil || sha == "" {
		return retirementFact{}, false, err
	}
	var record retirementRecord
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		return retirementFact{}, false, fmt.Errorf("decode retirement %s: %w", ref, err)
	}
	if err := validateRetirementRecord(ref, record); err != nil {
		return retirementFact{}, false, err
	}
	return retirementFact{Ref: ref, SHA: sha, Record: record}, true, nil
}

func recordRetirement(repoDir string, record retirementRecord) (retirementFact, error) {
	ref := retirementRef(record.Branch, record.Revision)
	if err := validateRetirementRecord(ref, record); err != nil {
		return retirementFact{}, err
	}
	if existing, found, err := readRetirement(repoDir, record.Branch, record.Revision); err != nil {
		return retirementFact{}, err
	} else if found {
		if existing.Record != record {
			return retirementFact{}, fmt.Errorf("retirement %s already records a different effect", existing.Ref)
		}
		return existing, nil
	}
	payload, err := retirementPayload(record)
	if err != nil {
		return retirementFact{}, err
	}
	if err := putBlobRef(repoDir, ref, payload, ""); err != nil {
		if errors.Is(err, errRefMoved) {
			existing, found, readErr := readRetirement(repoDir, record.Branch, record.Revision)
			if readErr == nil && found && existing.Record == record {
				return existing, nil
			}
		}
		return retirementFact{}, fmt.Errorf("record retirement: %w", err)
	}
	return retirementFact{
		Ref: retirementRef(record.Branch, record.Revision),
		SHA: blobSHA(payload), Record: record,
	}, nil
}

func landRetirement(repoDir string, fact retirementFact) (retirementFact, error) {
	if fact.Record.State == "landed" {
		return fact, nil
	}
	landed := fact.Record
	landed.State = "landed"
	payload, err := retirementPayload(landed)
	if err != nil {
		return retirementFact{}, err
	}
	if err := putBlobRef(repoDir, fact.Ref, payload, fact.SHA); err != nil {
		if errors.Is(err, errRefMoved) {
			current, found, readErr := readRetirement(repoDir, landed.Branch, landed.Revision)
			if readErr == nil && found && current.Record == landed {
				return current, nil
			}
		}
		return retirementFact{}, fmt.Errorf("land retirement: %w", err)
	}
	return retirementFact{Ref: fact.Ref, SHA: blobSHA(payload), Record: landed}, nil
}

func listRetirements(repoDir string) ([]retirementFact, error) {
	out, err := gitCommand(repoDir, "ls-remote", "origin", retirementRefPrefix+"*")
	if err != nil {
		return nil, err
	}
	byBranch := make(map[string]string)
	byItem := make(map[string]string)
	var facts []retirementFact
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], retirementRefPrefix) {
			continue
		}
		sha, body, err := getBlobRef(repoDir, fields[1])
		if err != nil {
			return nil, err
		}
		var record retirementRecord
		if err := json.Unmarshal([]byte(body), &record); err != nil {
			return nil, fmt.Errorf("decode retirement %s: %w", fields[1], err)
		}
		if err := validateRetirementRecord(fields[1], record); err != nil {
			return nil, err
		}
		if prior := byBranch[record.Branch]; prior != "" {
			return nil, fmt.Errorf("retirement branch %s has conflicting facts %s and %s", record.Branch, prior, fields[1])
		}
		byBranch[record.Branch] = fields[1]
		if prior := byItem[record.ItemID]; prior != "" {
			return nil, fmt.Errorf("retirement item %s has conflicting facts %s and %s", record.ItemID, prior, fields[1])
		}
		byItem[record.ItemID] = fields[1]
		facts = append(facts, retirementFact{Ref: fields[1], SHA: sha, Record: record})
	}
	return facts, nil
}

func retirementItemIDs(repoDir string) ([]string, error) {
	facts, err := listRetirements(repoDir)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(facts))
	for _, fact := range facts {
		ids = append(ids, fact.Record.ItemID)
	}
	return ids, nil
}

func dropRetirement(repoDir string, fact retirementFact) error {
	if err := deleteRef(repoDir, fact.Ref, fact.SHA); err != nil {
		return fmt.Errorf("drop retirement: %w", err)
	}
	return nil
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
