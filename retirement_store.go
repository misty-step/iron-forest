package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var errRetirementStale = errors.New("retirement intent is stale")
var errRetirementPreparation = errors.New("retirement preparation reset")
var errRetirementEvidenceInvalid = errors.New("durable retirement evidence is invalid")
var errRetirementRecoveryHard = errors.New("retirement recovery requires operator repair")

const retirementRefPrefix = "refs/forest/retirement/"

type retirementRecord struct {
	Branch       string `json:"branch"`
	Revision     string `json:"revision"`
	ItemID       string `json:"item_id"`
	Transport    string `json:"transport"`
	Strategy     string `json:"strategy"`
	Title        string `json:"title"`
	State        string `json:"state"`
	Agent        string `json:"agent"`
	Model        string `json:"model"`
	DefSHA       string `json:"def_sha"`
	BuiltComment bool   `json:"built_comment,omitempty"`
}

type retirementFact struct {
	Ref     string
	SHA     string
	Record  retirementRecord
	ReadErr error
}

func sanitizeRetirementRecord(record retirementRecord) (retirementRecord, error) {
	if secretShaped(record.Branch) || secretShaped(record.ItemID) {
		return retirementRecord{}, errors.New("retirement control identity has credential-shaped text")
	}
	record.Title = redactSecretShaped(record.Title)
	record.Agent = redactSecretShaped(record.Agent)
	record.Model = redactSecretShaped(record.Model)
	return record, nil
}

func retirementRef(branch, revision string) string {
	return retirementRefPrefix + encodeRefComponent(branch) + "/" + encodeRefComponent(revision)
}

func retirementPayload(record retirementRecord) (string, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return "", fmt.Errorf("encode retirement: %w", err)
	}
	body := string(payload)
	if secretShaped(body) {
		return "", errors.New("encode retirement: credential-shaped text remains")
	}
	return body, nil
}

func validateRetirementRecord(ref string, record retirementRecord) error {
	if secretShaped(record.Branch) || secretShaped(record.ItemID) ||
		secretShaped(record.Title) || secretShaped(record.Agent) || secretShaped(record.Model) {
		return errors.New("retirement evidence contains credential-shaped text")
	}
	validState := record.State == "preparing" || record.State == "observed" ||
		record.State == "pending" || record.State == "landed"
	if (record.Transport != "git" && record.Transport != "host") ||
		(record.Strategy != "squash" && record.Strategy != "ff") ||
		!validState ||
		(record.Transport == "host" && record.Strategy != "squash") ||
		(record.Transport == "git" && record.State != "landed") ||
		((record.State == "preparing" || record.State == "observed") && record.Transport != "host") {
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
	if record.State == "preparing" || record.State == "observed" {
		if record.Agent != "" || record.Model != "" || record.DefSHA != "" {
			return fmt.Errorf("retirement %s has attribution before a durable Verdict", ref)
		}
	} else if record.Agent == "" || record.Model == "" || !validHex(record.DefSHA, 8) {
		return fmt.Errorf("retirement %s has invalid agent attribution", ref)
	}
	if retirementRef(record.Branch, record.Revision) != ref {
		return fmt.Errorf("retirement %s content does not match its ref", ref)
	}
	return nil
}

func retirementMaterial(record retirementRecord, expectedRef string) (retirementFact, string, error) {
	record, err := sanitizeRetirementRecord(record)
	if err != nil {
		return retirementFact{}, "", err
	}
	ref := retirementRef(record.Branch, record.Revision)
	if expectedRef != "" {
		ref = expectedRef
	}
	if err := validateRetirementRecord(ref, record); err != nil {
		return retirementFact{}, "", err
	}
	payload, err := retirementPayload(record)
	if err != nil {
		return retirementFact{}, "", err
	}
	return retirementFact{Ref: ref, SHA: blobSHA(payload), Record: record}, payload, nil
}

func prepareRetirement(repoDir string, record retirementRecord) (retirementFact, error) {
	fact, payload, err := retirementMaterial(record, "")
	if err != nil {
		return retirementFact{}, err
	}
	fact.SHA, err = writeBlob(repoDir, payload)
	return fact, err
}

func readRetirement(repoDir, branch, revision string) (retirementFact, bool, error) {
	ref := retirementRef(branch, revision)
	sha, body, err := getBlobRef(repoDir, ref)
	if err != nil {
		return retirementFact{}, false, fmt.Errorf("%w: read retirement %s: %v",
			errFlowRetryable, ref, err)
	}
	if sha == "" {
		return retirementFact{}, false, nil
	}
	var record retirementRecord
	if err := json.Unmarshal([]byte(body), &record); err != nil {
		return retirementFact{}, false, fmt.Errorf("%w: decode retirement %s: %v",
			errRetirementEvidenceInvalid, ref, err)
	}
	if err := validateRetirementRecord(ref, record); err != nil {
		return retirementFact{}, false, fmt.Errorf("%w: %v", errRetirementEvidenceInvalid, err)
	}
	return retirementFact{Ref: ref, SHA: sha, Record: record}, true, nil
}

// recordRetirement stores facts created after the Builder comment completed.
// Pre-comment observation uses recordRetirementExact so recovery cannot lose
// its Builder exclusion while the Tracker is unavailable.
func recordRetirement(repoDir string, record retirementRecord) (retirementFact, error) {
	record.BuiltComment = true
	return recordRetirementExact(repoDir, record)
}

func recordRetirementExact(repoDir string, record retirementRecord) (retirementFact, error) {
	fact, payload, err := retirementMaterial(record, "")
	if err != nil {
		return retirementFact{}, err
	}
	if existing, found, err := readRetirement(repoDir, record.Branch, record.Revision); err != nil {
		return retirementFact{}, err
	} else if found {
		if existing.Record != fact.Record {
			return retirementFact{}, fmt.Errorf("%w: retirement %s already records a different effect",
				errRetirementEvidenceInvalid, existing.Ref)
		}
		return existing, nil
	}
	if err := putBlobRef(repoDir, fact.Ref, payload, ""); err != nil {
		if errors.Is(err, errRefMoved) {
			existing, found, readErr := readRetirement(repoDir, fact.Record.Branch, fact.Record.Revision)
			if readErr != nil {
				return retirementFact{}, readErr
			}
			if found && existing.Record == fact.Record {
				return existing, nil
			}
			if found {
				return retirementFact{}, fmt.Errorf("%w: retirement %s records a conflicting effect",
					errRetirementEvidenceInvalid, existing.Ref)
			}
		}
		return retirementFact{}, fmt.Errorf("%w: record retirement: %v", errFlowRetryable, err)
	}
	return fact, nil
}

func replaceRetirement(repoDir string, fact retirementFact, next retirementRecord) (retirementFact, error) {
	nextFact, payload, err := retirementMaterial(next, fact.Ref)
	if err != nil {
		return retirementFact{}, err
	}
	if err := putBlobRef(repoDir, fact.Ref, payload, fact.SHA); err != nil {
		if errors.Is(err, errRefMoved) {
			current, found, readErr := readRetirement(repoDir, next.Branch, next.Revision)
			if readErr != nil {
				return retirementFact{}, readErr
			}
			if found && current.Record == nextFact.Record {
				return current, nil
			}
			if found {
				return retirementFact{}, fmt.Errorf("%w: retirement %s changed to a conflicting effect",
					errRetirementEvidenceInvalid, current.Ref)
			}
		}
		return retirementFact{}, fmt.Errorf("%w: replace retirement: %v", errFlowRetryable, err)
	}
	return nextFact, nil
}

func moveRetirement(repoDir string, fact retirementFact, next retirementRecord) (retirementFact, error) {
	nextFact, err := prepareRetirement(repoDir, next)
	if err != nil {
		return retirementFact{}, err
	}
	if nextFact.Ref == fact.Ref {
		return replaceRetirement(repoDir, fact, next)
	}
	err = git(repoDir, "push", "--no-verify", "--atomic",
		"--force-with-lease="+fact.Ref+":"+fact.SHA,
		"--force-with-lease="+nextFact.Ref+":",
		"origin", ":"+fact.Ref, nextFact.SHA+":"+nextFact.Ref)
	if err == nil {
		return nextFact, nil
	}
	current, found, readErr := readRetirement(repoDir, next.Branch, next.Revision)
	if readErr != nil {
		return retirementFact{}, readErr
	}
	oldSHA, _, oldErr := getBlobRef(repoDir, fact.Ref)
	if oldErr != nil {
		return retirementFact{}, fmt.Errorf("%w: inspect moved retirement: %v", errFlowRetryable, oldErr)
	}
	if found && current.Record == nextFact.Record && oldSHA == "" {
		return current, nil
	}
	if found && current.Record != nextFact.Record || oldSHA != "" && oldSHA != fact.SHA {
		return retirementFact{}, fmt.Errorf("%w: retirement move encountered conflicting evidence",
			errRetirementEvidenceInvalid)
	}
	return retirementFact{}, fmt.Errorf("%w: move retirement: %v", errFlowRetryable, err)
}

func scanRetirements(repoDir string) ([]retirementFact, error) {
	out, err := gitCommand(repoDir, "ls-remote", "origin", retirementRefPrefix+"*")
	if err != nil {
		return nil, err
	}
	byBranch := make(map[string]int)
	byItem := make(map[string]int)
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
		if sha == "" {
			continue
		}
		fact := retirementFact{Ref: fields[1], SHA: sha}
		if err := json.Unmarshal([]byte(body), &fact.Record); err != nil {
			fact.ReadErr = fmt.Errorf("%w: decode retirement payload: %v",
				errRetirementEvidenceInvalid, err)
		} else if err := validateRetirementRecord(fields[1], fact.Record); err != nil {
			fact.ReadErr = fmt.Errorf("%w: %v", errRetirementEvidenceInvalid, err)
		}
		facts = append(facts, fact)
		if fact.ReadErr != nil {
			continue
		}
		current := len(facts) - 1
		if prior, found := byBranch[fact.Record.Branch]; found {
			conflict := fmt.Errorf("%w: retirement branch has conflicting facts",
				errRetirementEvidenceInvalid)
			facts[prior].ReadErr = conflict
			facts[current].ReadErr = conflict
		} else {
			byBranch[fact.Record.Branch] = current
		}
		if prior, found := byItem[fact.Record.ItemID]; found {
			conflict := fmt.Errorf("%w: retirement Item %s has conflicting facts",
				errRetirementEvidenceInvalid, fact.Record.ItemID)
			facts[prior].ReadErr = conflict
			facts[current].ReadErr = conflict
		} else {
			byItem[fact.Record.ItemID] = current
		}
	}
	return facts, nil
}

func retirementRefIdentity(ref string) (branch, id string, ok bool) {
	encoded := strings.TrimPrefix(ref, retirementRefPrefix)
	branchHex, _, found := strings.Cut(encoded, "/")
	if !found {
		return "", "", false
	}
	raw, err := hex.DecodeString(branchHex)
	if err != nil {
		return "", "", false
	}
	branch = string(raw)
	name := strings.TrimPrefix(branch, BranchPrefix)
	dash := strings.IndexByte(name, '-')
	if !strings.HasPrefix(branch, BranchPrefix) || dash == 0 || secretShaped(branch) {
		return "", "", false
	}
	segment := name
	if dash > 0 {
		segment = name[:dash]
	}
	id = decodeBranchID(segment)
	if validateTrackerItemID(id) != nil || encodeBranchID(id) != segment || secretShaped(id) {
		return "", "", false
	}
	return branch, id, true
}

func retirementItemIDs(repoDir string) ([]string, error) {
	facts, err := scanRetirements(repoDir)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(facts))
	for _, fact := range facts {
		if fact.ReadErr == nil {
			ids = append(ids, fact.Record.ItemID)
			continue
		}
		_, id, ok := retirementRefIdentity(fact.Ref)
		if !ok {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}
