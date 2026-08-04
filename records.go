package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// runRecord is the durable ledger line for one chewed item. appends go to
// .forest/runs.jsonl; append-only by construction.
type runRecord struct {
	Time     string  `json:"time"`
	RunID    string  `json:"run_id"`
	Issue    int     `json:"issue"`
	Branch   string  `json:"branch"`
	PRURL    string  `json:"pr_url"`
	Status   string  `json:"status"`
	CostUSD  float64 `json:"cost_usd"`
	TokensIn int64   `json:"tokens_in"`
	TokOut   int64   `json:"tokens_out"`
	Error    string  `json:"error,omitempty"`
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
