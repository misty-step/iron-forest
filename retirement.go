package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

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
