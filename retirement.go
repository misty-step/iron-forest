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
var errRetirementPreparation = errors.New("retirement preparation released")

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
	record, err := sanitizeRetirementRecord(record)
	if err != nil {
		return retirementFact{}, err
	}
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
	record, err := sanitizeRetirementRecord(record)
	if err != nil {
		return retirementFact{}, err
	}
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

func dropStaleRetirement(repoDir string, fact retirementFact, cause error) error {
	if err := dropRetirement(repoDir, fact); err != nil {
		return fmt.Errorf("retirement stale: %v; drop stale intent: %w", cause, err)
	}
	return fmt.Errorf("%w: %v", errRetirementStale, cause)
}

func dropPreparationRetirement(repoDir string, fact retirementFact, cause error) error {
	if err := dropRetirement(repoDir, fact); err != nil {
		return fmt.Errorf("retirement preparation rejected: %v; drop preparation: %w", cause, err)
	}
	return fmt.Errorf("%w: %v", errRetirementPreparation, cause)
}

// recordPendingHostRetirement is the one constructor for a durable pending Host fact.
func recordPendingHostRetirement(cfg Config, repoDir, branch, reviewed string, it Item, agent, model, defSHA string) (retirementFact, error) {
	return recordRetirement(repoDir, retirementRecord{
		Branch: branch, Revision: reviewed, ItemID: it.ID, Transport: "host",
		Strategy: cfg.Flows.Verifier.Merge, Title: it.Title, State: "pending",
		Agent: agent, Model: model, DefSHA: defSHA,
	})
}

func ensureHostProjection(cfg Config, branch, reviewed string, it Item) error {
	_, _, err := projectBranch(cfg, it, branch,
		fmt.Sprintf("Recovered Projection for item #%s: %s.\n", it.ID, it.Title), reviewed)
	return err
}

func recordHostRetirement(cfg Config, repoDir, branch, reviewed string, it Item, verdict verdictNote) error {
	if !cfg.Projection.MergeViaHost || verdict.Verdict != "approve" {
		return nil
	}
	if err := ensureHostProjection(cfg, branch, reviewed, it); err != nil {
		return err
	}
	_, err := recordPendingHostRetirement(cfg, repoDir, branch, reviewed, it,
		verdict.Reviewer, verdict.Model, verdict.DefSHA)
	return err
}

func recoverHostMergedProjection(cfg Config, repoDir, branch, reviewed string, it Item, out Outcome) (Outcome, error) {
	verdict, hasVerdict, verr := readVerdict(repoDir, reviewed)
	checks, hasChecks, cerr := readChecks(repoDir, reviewed)
	if verr != nil || cerr != nil || !hasVerdict || !hasChecks ||
		verdict.Verdict != "approve" || checks.Status != "pass" {
		out.Status = "merge_failed"
		return out, fmt.Errorf("Host merged Revision %s without durable factory approval and passing Checks", reviewed)
	}
	out.Agent = verdict.Reviewer
	out.Model = verdict.Model
	out.DefSHA = verdict.DefSHA
	out.Verdict = verdict.Verdict
	recoveryAgent := &Agent{Name: verdict.Reviewer, Model: verdict.Model, DefSHA: verdict.DefSHA}
	if err := mergeVerified(cfg, repoDir, branch, reviewed, it, recoveryAgent); err != nil {
		if errors.Is(err, errHostMergePending) {
			out.Status = "merge_pending"
			return out, nil
		}
		out.Status = "merge_failed"
		return out, err
	}
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

func mergeHostPath(cfg Config, repoDir, branch, reviewed string, it Item, a *Agent) error {
	if err := ensureHostProjection(cfg, branch, reviewed, it); err != nil {
		return err
	}
	fact, err := recordPendingHostRetirement(cfg, repoDir, branch, reviewed, it,
		a.Name, a.Model, a.DefSHA)
	if err != nil {
		return err
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
	if err := git(mergeDir, "push", "--atomic",
		"--force-with-lease=refs/heads/master:"+masterTip,
		"--force-with-lease=refs/heads/"+branch+":"+reviewed,
		"--force-with-lease="+fact.Ref+":",
		"origin", "HEAD:master", reviewed+":refs/heads/"+branch, fact.SHA+":"+fact.Ref); err != nil {
		return fmt.Errorf("merge: push: %w", err)
	}
	return finishRetirement(cfg, repoDir, fact, it)
}
func recoverRetirementFact(cfg Config, repoDir string, fact retirementFact, it Item) error {
	record := fact.Record
	if record.ItemID != it.ID || record.Branch == "" || record.Revision == "" {
		return fmt.Errorf("retirement %s does not match item %q", fact.Ref, it.ID)
	}
	if record.State == "pending" {
		if record.Transport != "host" {
			return fmt.Errorf("retirement %s is pending without Host transport", fact.Ref)
		}
		verdict, hasVerdict, verdictErr := readVerdict(repoDir, record.Revision)
		checks, hasChecks, checksErr := readChecks(repoDir, record.Revision)
		if verdictErr != nil || checksErr != nil || !hasVerdict || verdict.Verdict != "approve" ||
			!hasChecks || checks.Status != "pass" || record.Agent != verdict.Reviewer ||
			record.Model != verdict.Model || record.DefSHA != verdict.DefSHA {
			return dropPreparationRetirement(repoDir, fact,
				fmt.Errorf("retirement %s lacks matching durable approve Verdict and passing Checks", fact.Ref))
		}
		var err error
		if cfg.Flows.Verifier.AutoMerge {
			err = projectMerge(cfg, record.Branch, record.Strategy, record.Revision)
		} else {
			var hostMerged bool
			hostMerged, _, err = inspectProjectMerge(cfg, record.Branch, record.Strategy, record.Revision)
			if err == nil && !hostMerged {
				return errHostMergePending
			}
		}
		if err != nil {
			if errors.Is(err, errHostMergeUnavailable) {
				return dropStaleRetirement(repoDir, fact, err)
			}
			if errors.Is(err, errHostMergePending) {
				return err
			}
			return fmt.Errorf("%w: %v", errHostMergePending, err)
		}
		var landErr error
		fact, landErr = landRetirement(repoDir, fact)
		if landErr != nil {
			return landErr
		}
	}
	return finishRetirement(cfg, repoDir, fact, it)
}

func finishRetirement(cfg Config, repoDir string, fact retirementFact, it Item) error {
	record := fact.Record
	// The durable retirement fact replaces the source branch as recovery
	// evidence. Delete the exact reviewed branch first, so an advanced branch is
	// refused before the Tracker item closes and a failed Close stays resumable.
	if err := deleteReviewedBranch(repoDir, record.Branch, record.Revision); err != nil {
		return err
	}
	// The marker excludes the item from Builder selection until both Tracker
	// retirement and its attempt cleanup finish.
	if err := trackerFor(cfg.Repo).Close(it.ID); err != nil {
		return fmt.Errorf("merge: close item: %w", err)
	}
	if err := dropAttempts(repoDir, "branch-"+record.Branch); err != nil {
		return fmt.Errorf("merge: drop attempt record: %w", err)
	}
	return dropRetirement(repoDir, fact)
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
