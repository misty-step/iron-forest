package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// mergedPending and mergedLanded are the two durable states of a merged fact.
// A fact starts pending when a host merge is committed to but not yet confirmed,
// and graduates to landed once the merge is known to have landed. The distinction
// is what keeps reconciliation from closing an item or deleting a branch for work
// whose merge never actually happened.
const (
	mergedPending = "pending"
	mergedLanded  = "landed"
)

// mergedNote is a durable repository fact recording the merge state of one
// subject. The git path records it as landed atomically with the master push.
// The host path records a pending claim before the host merge and graduates it
// to landed after, so a flow can tell a merged subject from fresh work even while
// the Tracker item is still open. The note keys by the opaque item id, the one
// identity both Builder and Verifier can resolve.
type mergedNote struct {
	Branch   string `json:"branch"`
	Revision string `json:"revision"`
	Time     string `json:"time"`
	State    string `json:"state"`
}

// landed reports whether a merged fact is confirmed to have landed. An empty
// state is treated as landed so a fact written before the state field existed (or
// by a test constructing a note directly) still finalises as before.
func (n mergedNote) landed() bool {
	return n.State == mergedLanded || n.State == ""
}

// mergedRefPrefix is the ref namespace holding one durable merged fact per item.
const mergedRefPrefix = "refs/forest/merged/"

// mergedRef returns the durable ref recording that one item id merged.
func mergedRef(id string) string {
	return mergedRefPrefix + encodeBranchID(id)
}

// markMerged records the durable merged fact for one item, or is a no-op when
// the fact already exists. It is idempotent so a retry after partial failure is
// safe: the first durable fact wins and later calls leave it untouched. The git
// path writes it as landed, so its existence already proves the merge landed.
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
		return nil
	}
	return writeMergedNote(repoDir, id, note)
}

// writeMergedNote writes one merged fact blob under the item's ref, stamped with
// the current time.
func writeMergedNote(repoDir, id string, note mergedNote) error {
	ref := mergedRef(id)
	note.Time = nowRFC()
	payload, err := json.Marshal(note)
	if err != nil {
		return fmt.Errorf("mark merged %s: encode: %w", id, err)
	}
	if err := putBlobRef(repoDir, ref, string(payload), ""); err != nil {
		return fmt.Errorf("mark merged %s: %w", id, err)
	}
	return nil
}

// mergedNoteFor returns the durable merged note for one item id, or false when
// the item carries no merged fact. A selector compares it against a branch's
// current head so a branch that advanced past the merged Revision is offered as
// fresh work instead of suppressed by an item-wide fact. It never interprets the
// note's state: callers that finalise consult landed.
func mergedNoteFor(repoDir, id string) (mergedNote, bool, error) {
	if id == "" {
		return mergedNote{}, false, nil
	}
	ref := mergedRef(id)
	sha, body, err := getBlobRef(repoDir, ref)
	if err != nil {
		return mergedNote{}, false, err
	}
	if sha == "" {
		return mergedNote{}, false, nil
	}
	var note mergedNote
	if err := json.Unmarshal([]byte(body), &note); err != nil {
		return mergedNote{}, false, fmt.Errorf("merged %s: decode: %w", id, err)
	}
	return note, true, nil
}

// markMergedClaim records a pending merged claim for one item, the durable guard
// a host-path merge writes before it commits to the host merge. It is idempotent
// and write-once: an existing claim of any state is left untouched. Because the
// host merge removes no durable proof until it lands, this claim is what keeps a
// committed-but-unconfirmed merge from ever looking like fresh work.
func markMergedClaim(repoDir, id string, note mergedNote) error {
	if id == "" {
		return fmt.Errorf("mark merged claim: empty id")
	}
	if _, ok, err := mergedNoteFor(repoDir, id); err != nil {
		return err
	} else if ok {
		return nil
	}
	note.State = mergedPending
	return writeMergedNote(repoDir, id, note)
}

// confirmMerged graduates a merged claim from pending to landed once the merge is
// known to have landed. It is idempotent and concurrency-safe: an already-landed
// fact is a no-op, an absent claim is an error, and if the compare-and-set loses
// to a concurrent pass the claim is re-read so it converges on landed rather than
// surfacing a stale-ref error.
func confirmMerged(repoDir, id string) error {
	if id == "" {
		return fmt.Errorf("confirm merged: empty id")
	}
	for {
		ref := mergedRef(id)
		sha, body, err := getBlobRef(repoDir, ref)
		if err != nil {
			return fmt.Errorf("confirm merged %s: %w", id, err)
		}
		if sha == "" {
			return fmt.Errorf("confirm merged %s: no claim recorded", id)
		}
		var note mergedNote
		if err := json.Unmarshal([]byte(body), &note); err != nil {
			return fmt.Errorf("confirm merged %s: decode: %w", id, err)
		}
		if note.landed() {
			return nil
		}
		note.State = mergedLanded
		payload, err := json.Marshal(note)
		if err != nil {
			return fmt.Errorf("confirm merged %s: encode: %w", id, err)
		}
		err = putBlobRef(repoDir, ref, string(payload), sha)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errRefMoved) {
			return fmt.Errorf("confirm merged %s: %w", id, err)
		}
		// A concurrent pass won the compare-and-set; re-read and converge.
	}
}

// dropMergedClaim removes a pending merged claim whose merge never happened. It
// is only ever called on a pending fact that the host confirms did not land, so
// it can never erase proof of a real merge or a pre-existing landed fact.
func dropMergedClaim(repoDir, id string) error {
	if id == "" {
		return nil
	}
	ref := mergedRef(id)
	sha, _, err := getBlobRef(repoDir, ref)
	if err != nil {
		return err
	}
	if sha == "" {
		return nil
	}
	if err := deleteRef(repoDir, ref, sha); err != nil {
		return fmt.Errorf("drop merged claim %s: %w", id, err)
	}
	return nil
}

// mergedOnHost reports whether the host actually merged a subject's pull request.
// It is the observable half of the host-merge protocol: a pending claim is
// resolved by asking the host whether the merge landed, never by guessing. gh can
// look a pull request up by its head branch, so the branch recorded in the claim
// is enough to observe its state without knowing the pull request number.
func mergedOnHost(cfg Config, branch string) (bool, error) {
	out, err := projectionCommand("pr", "view", branch, "-R", cfg.Repo, "--json", "state")
	if err != nil {
		return false, err
	}
	var raw struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return false, fmt.Errorf("decode host merge state: %w", err)
	}
	return strings.EqualFold(raw.State, "merged"), nil
}

// mergedIDs returns the set of opaque item ids that carry a durable merged fact.
// A single listing covers every item, so a selector consults it once per pass
// instead of probing one ref per open item. Both pending and landed facts exclude
// a subject from eligibility: a committed-but-unconfirmed merge is never fresh
// work either.
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
// without operator action.
//
// It finalises only facts that landed. A pending fact is a committed-but-unconfirmed
// host merge: reconciliation resolves it by observing the host, graduating it to
// landed when the pull request actually merged and rolling the premature claim
// back when it did not. A pending claim is never closed or branch-deleted, so a
// crash or ambiguous host/network error before the merge lands can never make
// restart close an item or delete a branch for work that never merged.
func reconcileMerged(cfg Config, repoDir string) error {
	refs, err := listRefs(repoDir, mergedRefPrefix)
	if err != nil {
		return err
	}
	for _, r := range refs {
		id := decodeBranchID(strings.TrimPrefix(r.Ref, mergedRefPrefix))
		note, ok, err := mergedNoteFor(repoDir, id)
		if err != nil {
			return fmt.Errorf("merged %s: %w", id, err)
		}
		if !ok {
			continue
		}
		if !note.landed() {
			if !cfg.Projection.Enabled {
				// No host to observe; leave the claim for a later pass so a
				// never-landed subject is never finalised.
				continue
			}
			merged, err := mergedOnHost(cfg, note.Branch)
			if err != nil {
				return fmt.Errorf("merged %s: observe host: %w", id, err)
			}
			if !merged {
				if err := dropMergedClaim(repoDir, id); err != nil {
					return fmt.Errorf("merged %s: roll back claim: %w", id, err)
				}
				continue
			}
			if err := confirmMerged(repoDir, id); err != nil {
				return fmt.Errorf("merged %s: confirm: %w", id, err)
			}
			note.State = mergedLanded
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
