package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	Issue         int    `json:"issue"`
	Branch        string `json:"branch"`
	PRURL         string `json:"pr_url"`
	Status        string `json:"status"`
	TokensIn      int64  `json:"tokens_in"`
	TokOut        int64  `json:"tokens_out"`
	Agent         string `json:"agent"`
	Model         string `json:"model"`
	BaseSHA       string `json:"base_sha"`
	DefSHA        string `json:"def_sha"`
	ReviewVerdict string `json:"review,omitempty"`
	Error         string `json:"error,omitempty"`
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
