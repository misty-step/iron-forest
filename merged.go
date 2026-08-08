package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// mergedNote is a durable repository fact recording that one subject merged.
// It is written only after the merge actually lands (master carries the work),
// and before the source branch is removed, so a flow can tell a merged subject
// from fresh work even while the Tracker item is still open. The note keys by
// the opaque item id, the one identity both Builder and Verifier can resolve.
type mergedNote struct {
	Branch   string `json:"branch"`
	Revision string `json:"revision"`
	Time     string `json:"time"`
}

// mergedRefPrefix is the ref namespace holding one durable merged fact per item.
const mergedRefPrefix = "refs/forest/merged/"

// mergedRef returns the durable ref recording that one item id merged.
func mergedRef(id string) string {
	return mergedRefPrefix + encodeBranchID(id)
}

// markMerged records the durable merged fact for one item, or is a no-op when
// the fact already exists. It is idempotent so a retry after partial failure is
// safe: the first durable fact wins and later calls leave it untouched.
func markMerged(repoDir, id string, note mergedNote) error {
	if id == "" {
		return fmt.Errorf("mark merged: empty id")
	}
	ref := mergedRef(id)
	sha, _, err := getBlobRef(repoDir, ref)
	if err != nil {
		return fmt.Errorf("mark merged %s: %w", id, err)
	}
	if sha != "" {
		// Already merged: the durable fact is write-once and cannot be undone.
		return nil
	}
	note.Time = nowRFC()
	payload, err := json.Marshal(note)
	if err != nil {
		return fmt.Errorf("mark merged %s: encode: %w", id, err)
	}
	if err := putBlobRef(repoDir, ref, string(payload), sha); err != nil {
		return fmt.Errorf("mark merged %s: %w", id, err)
	}
	return nil
}

// mergedIDs returns the set of opaque item ids that carry a durable merged fact.
// A single listing covers every item, so a selector consults it once per pass
// instead of probing one ref per open item.
func mergedIDs(repoDir string) (map[string]bool, error) {
	refs, err := listRefs(repoDir, mergedRefPrefix)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(refs))
	for _, r := range refs {
		set[decodeBranchID(strings.TrimPrefix(r.Ref, mergedRefPrefix))] = true
	}
	return set, nil
}

// reconcileMerged finds every subject with a durable merged fact whose
// finalisation is incomplete — the Tracker item still open, the source branch
// still present, or the attempt record still present — and completes it. Each
// effect is idempotent, so a partial failure is finished here on a later pass
// without operator action. It never reverses a merge: the durable fact is the
// authority that the work already landed.
func reconcileMerged(cfg Config, repoDir string) error {
	refs, err := listRefs(repoDir, mergedRefPrefix)
	if err != nil {
		return err
	}
	for _, r := range refs {
		id := decodeBranchID(strings.TrimPrefix(r.Ref, mergedRefPrefix))
		_, body, err := getBlobRef(repoDir, r.Ref)
		if err != nil {
			return fmt.Errorf("merged %s: %w", id, err)
		}
		var note mergedNote
		if err := json.Unmarshal([]byte(body), &note); err != nil {
			return fmt.Errorf("merged %s: decode: %w", id, err)
		}
		if err := trackerFor(cfg.Repo).Close(id); err != nil {
			return fmt.Errorf("merged %s: close item: %w", id, err)
		}
		if err := deleteBranchIfPresent(repoDir, note.Branch, note.Revision); err != nil {
			return fmt.Errorf("merged %s: delete branch: %w", id, err)
		}
		if err := dropAttempts(repoDir, "branch-"+note.Branch); err != nil {
			return fmt.Errorf("merged %s: drop attempts: %w", id, err)
		}
	}
	return nil
}
