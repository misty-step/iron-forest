package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errRetirementStale = errors.New("retirement intent is stale")
var errRetirementPreparation = errors.New("retirement preparation reset")
var errRetirementEvidenceInvalid = errors.New("durable retirement evidence is invalid")
var errRetirementRecoveryHard = errors.New("retirement recovery requires operator repair")

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

func recordRetirement(repoDir string, record retirementRecord) (retirementFact, error) {
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

func landRetirement(repoDir string, fact retirementFact) (retirementFact, error) {
	if fact.Record.State == "landed" {
		return fact, nil
	}
	landed := fact.Record
	landed.State = "landed"
	return replaceRetirement(repoDir, fact, landed)
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

func listRetirements(repoDir string) ([]retirementFact, error) {
	facts, err := scanRetirements(repoDir)
	if err != nil {
		return nil, err
	}
	for _, fact := range facts {
		if fact.ReadErr != nil {
			return nil, fact.ReadErr
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
	if !strings.HasPrefix(branch, BranchPrefix) || dash <= 0 || secretShaped(branch) {
		return "", "", false
	}
	id = decodeBranchID(name[:dash])
	if id == "" || secretShaped(id) {
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
		if fact.ReadErr != nil {
			return nil, fmt.Errorf("%w: %v", errRetirementEvidenceInvalid, fact.ReadErr)
		}
		ids = append(ids, fact.Record.ItemID)
	}
	return ids, nil
}

func dropRetirement(repoDir string, fact retirementFact) error {
	sha, _, err := getBlobRef(repoDir, fact.Ref)
	if err != nil {
		return fmt.Errorf("%w: inspect retirement deletion: %v", errFlowRetryable, err)
	}
	if sha == "" {
		return nil
	}
	if sha != fact.SHA {
		return fmt.Errorf("%w: retirement changed before deletion", errFlowRetryable)
	}
	if err := deleteRef(repoDir, fact.Ref, fact.SHA); err != nil {
		return fmt.Errorf("%w: delete retirement: %v", errFlowRetryable, err)
	}
	return nil
}

// recordPreparingHostRetirement publishes the recovery identity before any Host
// request can exist. A preparing fact blocks duplicate Builder work but still
// lets the live branch reach the Verifier.
func recordPreparingHostRetirement(cfg Config, repoDir, branch, reviewed string, it Item) (retirementFact, error) {
	next := retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: it.ID, Transport: "host",
		Strategy: cfg.Flows.Verifier.Merge, Title: it.Title, State: "preparing",
	}
	if fact, found, err := readRetirement(repoDir, branch, reviewed); err != nil {
		return retirementFact{}, err
	} else if found {
		if fact.Record.ItemID != it.ID {
			return retirementFact{}, fmt.Errorf("%w: retirement %s does not match item %q",
				errHostMergeUnavailable, fact.Ref, it.ID)
		}
		return fact, nil
	}
	facts, err := scanRetirements(repoDir)
	if err != nil {
		return retirementFact{}, err
	}
	for _, fact := range facts {
		if fact.ReadErr != nil {
			if _, id, ok := retirementRefIdentity(fact.Ref); ok && id == it.ID {
				return retirementFact{}, fact.ReadErr
			}
			continue
		}
		if fact.Record.Branch != branch && fact.Record.ItemID != it.ID {
			continue
		}
		if fact.Record.Branch != branch || fact.Record.ItemID != it.ID ||
			fact.Record.State != "preparing" {
			return retirementFact{}, fmt.Errorf("%w: retirement %s already owns branch %q or item %q",
				errHostMergeUnavailable, fact.Ref, branch, it.ID)
		}
		head, err := branchHead(repoDir, branch)
		if err != nil || head != reviewed {
			return retirementFact{}, fmt.Errorf("%w: %w: retirement %s cannot move from %s to %s without the exact branch head",
				errHostMergeUnavailable, errHostRevisionMoved, fact.Ref, fact.Record.Revision, reviewed)
		}
		merged, err := mergedProjectionPRs(cfg, branch)
		if err != nil {
			return retirementFact{}, fmt.Errorf("inspect prior Host Revision: %w", err)
		}
		for _, pr := range merged {
			if pr.HeadRefOID != fact.Record.Revision {
				continue
			}
			observed, err := observeHostRetirement(repoDir, fact)
			if err != nil {
				return retirementFact{}, err
			}
			return observed, fmt.Errorf("%w: Host already merged prior Revision %s",
				errHostMergePending, fact.Record.Revision)
		}
		prs, err := openProjectionPR(cfg, branch)
		if err != nil {
			return retirementFact{}, fmt.Errorf("inspect advanced Host Projection: %w", err)
		}
		if len(prs) == 0 {
			return moveRetirement(repoDir, fact, next)
		}
		if prs[0].HeadRefOID == "" || prs[0].HeadRefOID != reviewed {
			return retirementFact{}, fmt.Errorf("%w: %w: Host Projection for %s reports Revision %s, want %s",
				errHostMergeUnavailable, errHostRevisionMoved, branch, prs[0].HeadRefOID, reviewed)
		}
		return moveRetirement(repoDir, fact, next)
	}
	return recordRetirement(repoDir, next)
}

func pendingHostRetirement(repoDir string, fact retirementFact, verdict verdictNote) (retirementFact, error) {
	if fact.Record.State == "landed" || fact.Record.State == "observed" {
		return fact, nil
	}
	if fact.Record.State != "preparing" && fact.Record.State != "pending" {
		return retirementFact{}, fmt.Errorf("retirement %s cannot become pending from %q", fact.Ref, fact.Record.State)
	}
	pending := fact.Record
	pending.State = "pending"
	pending.Agent = verdict.Reviewer
	pending.Model = verdict.Model
	pending.DefSHA = verdict.DefSHA
	if pending == fact.Record {
		return fact, nil
	}
	return replaceRetirement(repoDir, fact, pending)
}

func resetHostRetirement(repoDir string, fact retirementFact, cause error) (retirementFact, error) {
	preparing := fact.Record
	preparing.State = "preparing"
	preparing.Agent = ""
	preparing.Model = ""
	preparing.DefSHA = ""
	next, err := replaceRetirement(repoDir, fact, preparing)
	if err != nil {
		return fact, err
	}
	return next, fmt.Errorf("%w: %v", errRetirementPreparation, cause)
}

func moveAdvancedHostRetirement(cfg Config, repoDir string, fact retirementFact) (retirementFact, bool, error) {
	head, err := branchHead(repoDir, fact.Record.Branch)
	if err != nil {
		return fact, false, fmt.Errorf("%w: inspect advanced retirement branch: %v",
			errFlowRetryable, err)
	}
	if head == fact.Record.Revision {
		return fact, false, nil
	}
	prs, err := openProjectionPR(cfg, fact.Record.Branch)
	if err != nil {
		return fact, false, err
	}
	if len(prs) != 1 || prs[0].HeadRefOID != head {
		return fact, false, nil
	}
	next := fact.Record
	next.Revision = head
	next.State = "preparing"
	next.Agent = ""
	next.Model = ""
	next.DefSHA = ""
	moved, err := moveRetirement(repoDir, fact, next)
	return moved, err == nil, err
}

func observeHostRetirement(repoDir string, fact retirementFact) (retirementFact, error) {
	switch fact.Record.State {
	case "observed", "landed":
		return fact, nil
	case "preparing", "pending":
	default:
		return retirementFact{}, fmt.Errorf("retirement %s cannot become observed from %q",
			fact.Ref, fact.Record.State)
	}
	observed := fact.Record
	observed.State = "observed"
	observed.Agent = ""
	observed.Model = ""
	observed.DefSHA = ""
	return replaceRetirement(repoDir, fact, observed)
}

// recordObservedHostRetirement preserves an exact Host merge that arrived
// before durable approval was readable. The fact blocks duplicate Builder work
// until recovery can join the merge observation with its Verdict and Checks.
func recordObservedHostRetirement(cfg Config, repoDir, branch, reviewed string, it Item) (retirementFact, error) {
	if fact, found, err := readRetirement(repoDir, branch, reviewed); err != nil {
		return retirementFact{}, err
	} else if found {
		if fact.Record.ItemID != it.ID {
			return retirementFact{}, fmt.Errorf("retirement %s does not match item %q", fact.Ref, it.ID)
		}
		return observeHostRetirement(repoDir, fact)
	}
	record := retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: it.ID, Transport: "host",
		Strategy: cfg.Flows.Verifier.Merge, Title: it.Title, State: "observed",
	}
	fact, err := recordRetirement(repoDir, record)
	if err == nil {
		return fact, nil
	}
	// Another checkout can publish the approval preparation between the read
	// and compare-and-set. Its exact fact owns recovery.
	if winner, found, readErr := readRetirement(repoDir, branch, reviewed); readErr == nil && found && winner.Record.ItemID == it.ID {
		return observeHostRetirement(repoDir, winner)
	}
	return retirementFact{}, err
}

func landObservedRetirement(repoDir string, fact retirementFact, verdict verdictNote) (retirementFact, error) {
	if fact.Record.State != "observed" {
		return retirementFact{}, fmt.Errorf("retirement %s is not an observed Host merge", fact.Ref)
	}
	landed := fact.Record
	landed.State = "landed"
	landed.Agent = verdict.Reviewer
	landed.Model = verdict.Model
	landed.DefSHA = verdict.DefSHA
	return replaceRetirement(repoDir, fact, landed)
}

func recordHostRetirement(
	cfg Config,
	repoDir, branch, reviewed string,
	it Item,
	verdict verdictNote,
	checks checksNote,
) error {
	if !cfg.Projection.MergeViaHost || verdict.Verdict != "approve" {
		return nil
	}
	fact, err := recordPreparingHostRetirement(cfg, repoDir, branch, reviewed, it)
	if err != nil {
		return err
	}
	if _, _, err := projectBranch(cfg, repoDir, it, branch,
		fmt.Sprintf("Recovered Projection for item #%s: %s.\n", it.ID, it.Title), reviewed); err != nil {
		return err
	}
	if err := projectVerdict(cfg, branch, reviewed, verdict, checks); err != nil {
		return err
	}
	_, err = pendingHostRetirement(repoDir, fact, verdict)
	return err
}

func recoverHostMergedProjection(cfg Config, repoDir, branch, reviewed string, it Item, out Outcome) (Outcome, error) {
	fact, err := recordObservedHostRetirement(cfg, repoDir, branch, reviewed, it)
	if err != nil {
		out.Status = "merge_failed"
		return out, err
	}
	record, err := recoverRetirement(cfg, repoDir, fact, it)
	if err != nil {
		if errors.Is(err, errHostMergePending) {
			out.Status = "merge_pending"
			return out, nil
		}
		out.Status = "merge_failed"
		return out, err
	}
	out.Agent = record.Agent
	out.Model = record.Model
	out.DefSHA = record.DefSHA
	out.Verdict = "approve"
	out.Status = "merged"
	return out, nil
}

// mergeVerified lands only the Revision that carried the approving Verdict.
// A retirement fact makes the multi-system effect resumable. Git writes its
// landed fact atomically with master. The host path writes pending intent first,
// then promotes it after the host reports the exact reviewed head as merged.
func mergeVerified(cfg Config, repoDir, branch, reviewed string, it Item, a *Agent) error {
	if fact, found, err := readRetirement(repoDir, branch, reviewed); err != nil {
		return err
	} else if found {
		return recoverRetirementFact(cfg, repoDir, fact, it)
	}
	if cfg.Projection.MergeViaHost {
		// The Host request's exact head is the revision fence. This path also
		// recovers after the Host merged and deleted the source branch.
		return mergeHostPath(cfg, repoDir, branch, reviewed, it, a)
	}
	if err := fenceMergeOnRevision(repoDir, branch, reviewed); err != nil {
		return err
	}
	return mergeGitPath(cfg, repoDir, branch, reviewed, it, a)
}

func mergeHostPath(cfg Config, repoDir, branch, reviewed string, it Item, _ *Agent) error {
	verdict, checks, err := readRetirementApproval(repoDir, reviewed)
	if err != nil {
		return err
	}
	if verdict.Verdict != "approve" || checks.Status != "pass" {
		return fmt.Errorf("%w: Host merge requires durable approval", errRetirementEvidenceInvalid)
	}
	if err := recordHostRetirement(cfg, repoDir, branch, reviewed, it, verdict, checks); err != nil {
		return err
	}
	fact, found, err := readRetirement(repoDir, branch, reviewed)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: Host retirement disappeared after publication", errFlowRetryable)
	}
	return recoverRetirementFact(cfg, repoDir, fact, it)
}

// mergeGitPath commits the retirement fact in the same atomic push that
// advances master. A retry therefore skips merge construction and resumes at
// Tracker retirement, including squash merges whose tree is already on master.
func mergeGitPath(cfg Config, repoDir, branch, reviewed string, it Item, a *Agent) error {
	if fact, found, err := readRetirement(repoDir, branch, reviewed); err != nil {
		return err
	} else if found {
		return recoverRetirementFact(cfg, repoDir, fact, it)
	}
	workspace := workspaceDir(repoDir)
	mergeDir := filepath.Join(workspace, "worktrees", "merge-"+slug(branch))
	if err := trackWorktree(mergeDir); err != nil {
		return fmt.Errorf("merge: %w", err)
	}
	defer cleanupWorktree(repoDir, mergeDir)
	_ = os.RemoveAll(mergeDir)
	if err := git(repoDir, "worktree", "prune"); err != nil {
		return fmt.Errorf("merge: prune: %w", err)
	}
	if err := git(repoDir, "fetch", "origin", "master"); err != nil {
		return fmt.Errorf("merge: fetch master: %w", err)
	}
	masterTip, err := gitOut(repoDir, "rev-parse", "origin/master")
	if err != nil {
		return fmt.Errorf("merge: origin/master: %w", err)
	}
	if err := git(repoDir, "worktree", "add", "--detach", mergeDir, "origin/master"); err != nil {
		return fmt.Errorf("merge: worktree: %w", err)
	}
	switch cfg.Flows.Verifier.Merge {
	case "squash":
		if err := git(mergeDir, "merge", "--squash", reviewed); err != nil {
			return fmt.Errorf("merge: squash: %w", err)
		}
		if err := gitCommit(mergeDir, a.Commit, fmt.Sprintf("forest: %s (#%s)", it.Title, it.ID)); err != nil {
			return fmt.Errorf("merge: commit: %w", err)
		}
	case "ff":
		if err := git(mergeDir, "merge", "--ff-only", reviewed); err != nil {
			return fmt.Errorf("merge: ff: %w", err)
		}
	default:
		return fmt.Errorf("merge: unsupported strategy %q", cfg.Flows.Verifier.Merge)
	}
	fact, err := prepareRetirement(repoDir, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: it.ID, Transport: "git",
		Strategy: cfg.Flows.Verifier.Merge, Title: it.Title, State: "landed",
		Agent: a.Name, Model: a.Model, DefSHA: a.DefSHA,
	})
	if err != nil {
		return err
	}
	if pushErr := git(mergeDir, "push", "--atomic",
		"--force-with-lease=refs/heads/master:"+masterTip,
		"--force-with-lease=refs/heads/"+branch+":"+reviewed,
		"--force-with-lease="+fact.Ref+":",
		"origin", "HEAD:master", reviewed+":refs/heads/"+branch, fact.SHA+":"+fact.Ref); pushErr != nil {
		current, found, readErr := readRetirement(repoDir, branch, reviewed)
		if readErr != nil {
			return readErr
		}
		if found {
			return recoverRetirementFact(cfg, repoDir, current, it)
		}
		head, present, headErr := lookupBranchHead(repoDir, branch)
		if headErr != nil {
			return fmt.Errorf("%w: reconcile merge push: %v", errFlowRetryable, headErr)
		}
		if !present || head != reviewed {
			return fmt.Errorf("%w: merge source branch moved after failed push", errRetirementStale)
		}
		return fmt.Errorf("%w: merge push: %v", errFlowRetryable, pushErr)
	}
	return finishRetirement(cfg, repoDir, fact, it)
}
func retirementEvidenceReadError(kind string, err error) error {
	if errors.Is(err, errNoteInvalid) {
		return fmt.Errorf("%w: read durable %s: %v", errRetirementEvidenceInvalid, kind, err)
	}
	return fmt.Errorf("%w: read durable %s: %v", errHostMergePending, kind, err)
}

func readRetirementApproval(repoDir, revision string) (verdictNote, checksNote, error) {
	if err := fetchNotes(repoDir); err != nil {
		return verdictNote{}, checksNote{}, fmt.Errorf("%w: refresh durable notes: %v", errHostMergePending, err)
	}
	verdict, _, err := readVerdict(repoDir, revision)
	if err != nil {
		return verdictNote{}, checksNote{}, retirementEvidenceReadError("Verdict", err)
	}
	checks, _, err := readChecks(repoDir, revision)
	if err != nil {
		return verdictNote{}, checksNote{}, retirementEvidenceReadError("Checks", err)
	}
	return verdict, checks, nil
}

func hostMergeAttemptLimit(cfg Config) int {
	if cfg.Flows.Fixer.Attempts < 1 {
		return 1
	}
	return cfg.Flows.Fixer.Attempts
}

func recordHostMergeHandoff(
	cfg Config,
	repoDir string,
	record retirementRecord,
	it Item,
	handoff error,
) error {
	if err := recordTerminalStall(repoDir, (verifierFlow{}).Name(),
		retirementSubjectKey(record.Branch), record.Revision); err != nil {
		return fmt.Errorf("%w: %v; record durable Host merge brake: %v",
			errRetirementRecoveryHard, handoff, err)
	}
	tracker := trackerFor(cfg.Repo)
	if err := tracker.SetTags(it.ID, []string{failedLabel}, nil); err != nil {
		return fmt.Errorf("%w: %v; publish Host merge handoff tag: %v",
			errRetirementRecoveryHard, handoff, err)
	}
	if err := publishMergeBlocked(tracker, it, record.Revision, handoff); err != nil {
		return fmt.Errorf("%w: %v; publish Host merge handoff: %v",
			errRetirementRecoveryHard, handoff, err)
	}
	return fmt.Errorf("%w: %v", errRetirementRecoveryHard, handoff)
}

func recordHostMergeRequestFailure(
	cfg Config,
	repoDir string,
	record retirementRecord,
	it Item,
	cause error,
) error {
	attempts, err := bumpAttempts(repoDir, "branch-"+record.Branch)
	if err != nil {
		handoff := fmt.Errorf("Host merge request for Revision %s failed before its attempt became durable",
			record.Revision)
		return fmt.Errorf("%w; attempt record: %v",
			recordHostMergeHandoff(cfg, repoDir, record, it, handoff), err)
	}
	if attempts < hostMergeAttemptLimit(cfg) {
		return fmt.Errorf("%w: %v", errHostMergePending, cause)
	}
	handoff := fmt.Errorf("Host merge request for Revision %s failed after %d attempts",
		record.Revision, attempts)
	return recordHostMergeHandoff(cfg, repoDir, record, it, handoff)
}

func recoverRetirementFact(cfg Config, repoDir string, fact retirementFact, it Item) error {
	_, err := recoverRetirement(cfg, repoDir, fact, it)
	return err
}

func recoverRetirement(cfg Config, repoDir string, fact retirementFact, it Item) (retirementRecord, error) {
	record := fact.Record
	if record.ItemID != it.ID || record.Branch == "" || record.Revision == "" {
		return record, fmt.Errorf("retirement %s does not match item %q", fact.Ref, it.ID)
	}
	if record.State == "preparing" || record.State == "pending" {
		if record.Transport != "host" {
			return record, fmt.Errorf("retirement %s is active without Host transport", fact.Ref)
		}
		starting := record.State
		hostMerged, _, hostErr := inspectProjectMerge(cfg, record.Branch, record.Strategy, record.Revision)
		if hostErr != nil {
			if errors.Is(hostErr, errHostRevisionMoved) {
				moved, changed, moveErr := moveAdvancedHostRetirement(cfg, repoDir, fact)
				if moveErr != nil {
					return record, moveErr
				}
				if changed {
					return moved.Record, fmt.Errorf("%w: Host Projection advanced to Revision %s",
						errRetirementPreparation, moved.Record.Revision)
				}
				return record, fmt.Errorf("%w: moved Host Projection has no exact migration target",
					errHostMergeUnavailable)
			}
			if errors.Is(hostErr, errHostMergeNoView) {
				if starting == "preparing" {
					head, found, headErr := lookupBranchHead(repoDir, record.Branch)
					if headErr != nil {
						return record, fmt.Errorf("%w: inspect preparing branch: %v", errHostMergePending, headErr)
					}
					if !found {
						return record, fmt.Errorf("%w: preparing branch %q has no Host view or remote Revision",
							errRetirementRecoveryHard, record.Branch)
					}
					if head != record.Revision {
						moved, moveErr := recordPreparingHostRetirement(cfg, repoDir, record.Branch, head, it)
						if moveErr != nil {
							return record, moveErr
						}
						return moved.Record, fmt.Errorf("%w: Host branch advanced to Revision %s before Projection",
							errRetirementPreparation, moved.Record.Revision)
					}
					_, merged, projectErr := projectBranch(cfg, repoDir, it, record.Branch,
						fmt.Sprintf("Recovered Projection for item #%s: %s.\n", it.ID, it.Title), record.Revision)
					if projectErr != nil {
						return record, retryableHostError(cfg, projectErr)
					}
					if merged {
						observed, observeErr := observeHostRetirement(repoDir, fact)
						if observeErr != nil {
							return record, observeErr
						}
						return observed.Record, errHostMergePending
					}
					return record, errRetirementPreparation
				}
				return record, fmt.Errorf("%w: %v", errHostMergePending, hostErr)
			}
			if errors.Is(hostErr, errHostMergeUnavailable) {
				return record, hostErr
			}
			return record, fmt.Errorf("%w: %v", errHostMergePending, hostErr)
		}
		if hostMerged {
			observed, err := observeHostRetirement(repoDir, fact)
			if err != nil {
				return record, err
			}
			fact = observed
			record = fact.Record
		}
		verdict, checks, err := readRetirementApproval(repoDir, record.Revision)
		if err != nil {
			return record, fmt.Errorf("retirement %s: %w", fact.Ref, err)
		}
		approved := verdict.Verdict == "approve" && checks.Status == "pass"
		matches := starting == "preparing" || record.Agent == verdict.Reviewer &&
			record.Model == verdict.Model && record.DefSHA == verdict.DefSHA
		if hostMerged {
			if !approved {
				return record, errHostMergePending
			}
			fact, err = landObservedRetirement(repoDir, fact, verdict)
			if err != nil {
				return record, err
			}
			record = fact.Record
		} else {
			if !approved {
				if starting == "pending" {
					next, resetErr := resetHostRetirement(repoDir, fact,
						fmt.Errorf("retirement %s lacks a durable approve Verdict and passing Checks", fact.Ref))
					return next.Record, resetErr
				}
				return record, errHostMergePending
			}
			if starting == "preparing" || !matches {
				fact, err = pendingHostRetirement(repoDir, fact, verdict)
				if err != nil {
					return record, err
				}
				record = fact.Record
			}
			if !cfg.Flows.Verifier.AutoMerge {
				return record, errHostMergePending
			}
			attempts, attemptsErr := readAttempts(repoDir, "branch-"+record.Branch)
			if attemptsErr != nil {
				return record, attemptsErr
			}
			if attempts >= hostMergeAttemptLimit(cfg) {
				handoff := fmt.Errorf("Host merge request for Revision %s failed after %d attempts",
					record.Revision, attempts)
				return record, recordHostMergeHandoff(cfg, repoDir, record, it, handoff)
			}
			if err := projectMerge(cfg, record.Branch, record.Strategy, record.Revision); err != nil {
				if errors.Is(err, errHostMergeUnavailable) {
					return record, err
				}
				if errors.Is(err, errHostMergeRequestFailed) {
					return record, recordHostMergeRequestFailure(cfg, repoDir, record, it, err)
				}
				return record, fmt.Errorf("%w: %v", errHostMergePending, err)
			}
			fact, err = landRetirement(repoDir, fact)
			if err != nil {
				return record, err
			}
			record = fact.Record
		}
	}
	if record.State == "observed" {
		verdict, checks, err := readRetirementApproval(repoDir, record.Revision)
		if err != nil {
			return record, fmt.Errorf("retirement %s: %w", fact.Ref, err)
		}
		approved := verdict.Verdict == "approve" && checks.Status == "pass"
		if !approved {
			return record, errHostMergePending
		}
		fact, err = landObservedRetirement(repoDir, fact, verdict)
		if err != nil {
			return record, err
		}
		record = fact.Record
	}
	return record, finishRetirement(cfg, repoDir, fact, it)
}
func finishRetirement(cfg Config, repoDir string, fact retirementFact, it Item) error {
	record := fact.Record
	// The durable retirement fact replaces the source branch as recovery
	// evidence. Delete the exact reviewed branch first, so an advanced branch is
	// refused before the Tracker item closes and a failed Close stays resumable.
	if err := deleteReviewedBranch(repoDir, record.Branch, record.Revision); err != nil {
		if errors.Is(err, errRetirementStale) {
			return err
		}
		return fmt.Errorf("%w: %v", errFlowRetryable, err)
	}
	// The marker excludes the item from Builder selection until both Tracker
	// retirement and its attempt cleanup finish.
	if err := trackerFor(cfg.Repo).Close(it.ID); err != nil {
		return fmt.Errorf("%w: merge: close item: %v", errFlowRetryable, err)
	}
	if err := dropAttempts(repoDir, "branch-"+record.Branch); err != nil {
		return fmt.Errorf("%w: merge: drop attempt record: %v", errFlowRetryable, err)
	}
	if err := dropRetirement(repoDir, fact); err != nil {
		return fmt.Errorf("%w: merge: drop retirement: %v", errFlowRetryable, err)
	}
	return nil
}

func deleteReviewedBranch(repoDir, branch, reviewed string) error {
	out, err := gitCommand(repoDir, "ls-remote", "origin", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("merge: inspect branch %s: %w", branch, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return nil
	}
	if fields[0] != reviewed {
		return fmt.Errorf("%w: merge: branch %s advanced to %s before retirement of %s", errRetirementStale, branch, fields[0], reviewed)
	}
	if err := deleteRef(repoDir, "refs/heads/"+branch, reviewed); err != nil {
		return fmt.Errorf("merge: delete branch %s (wanted %s): %w", branch, reviewed, err)
	}
	return nil
}
