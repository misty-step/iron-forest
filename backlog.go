package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// comment is one tracker comment in source order.
type comment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

// ghJSON runs the host CLI once. Tests replace it to stub the tracker without
// invoking the host binary.
var ghJSON = func(args ...string) ([]byte, error) {
	return runOutput(exec.Command("gh", args...))
}

// githubTracker adapts the GitHub CLI to the Tracker port. It is the only
// evidence the controller holds that the work source is GitHub: ListOpen, Get,
// and the mutating effects all round-trip an opaque string id. A second work
// source pairs a Tracker of its own with this same shape.
type githubTracker struct {
	repo string
}

// ListOpen returns every open item on the host.
func (t githubTracker) ListOpen() ([]Item, error) {
	out, err := ghJSON("issue", "list", "-R", t.repo, "--state", "open",
		"--json", "number,title,body,updatedAt,comments,labels", "--limit", strconv.Itoa(int(^uint(0)>>1)))
	if err != nil {
		return nil, fmt.Errorf("%w: list open items: %v", errTrackerUnavailable, err)
	}
	var raw []issueJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("%w: decode Tracker items: %v", errTrackerEvidenceInvalid, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%w: decode Tracker items: response is null", errTrackerEvidenceInvalid)
	}
	items := make([]Item, 0, len(raw))
	for _, it := range raw {
		items = append(items, it.asItem())
	}
	return items, nil
}

// Get reads one item by its opaque id.
func (t githubTracker) Get(id string) (Item, error) {
	out, err := ghJSON("issue", "view", "-R", t.repo, id,
		"--json", "number,title,body,updatedAt,comments,labels")
	if err != nil {
		return Item{}, fmt.Errorf("%w: get item: %v", errTrackerUnavailable, err)
	}
	var raw issueJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return Item{}, fmt.Errorf("%w: decode Tracker item: %v", errTrackerEvidenceInvalid, err)
	}
	return raw.asItem(), nil
}

func (t githubTracker) Comment(id, body string) error {
	_, err := ghJSON("issue", "comment", "-R", t.repo, id, "--body", body)
	if err != nil {
		return fmt.Errorf("%w: comment on item: %v", errTrackerUnavailable, err)
	}
	return nil
}

// Close idempotently closes one item. If the close command fails after the Host
// accepted it, an exact state read makes a recovery retry safe.
func (t githubTracker) Close(id string) error {
	_, closeErr := ghJSON("issue", "close", "-R", t.repo, id)
	if closeErr == nil {
		return nil
	}
	out, viewErr := ghJSON("issue", "view", id, "-R", t.repo, "--json", "state")
	if viewErr != nil {
		return fmt.Errorf("%w: close item: %v; reconcile: %v", errTrackerUnavailable, closeErr, viewErr)
	}
	var state struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		return fmt.Errorf("%w: decode closed Item state: %v", errTrackerEvidenceInvalid, err)
	}
	switch strings.ToLower(strings.TrimSpace(state.State)) {
	case "closed":
		return nil
	case "open":
		return fmt.Errorf("%w: %w: close item: %v",
			errTrackerUnavailable, errTrackerEffectNotApplied, closeErr)
	case "":
		return fmt.Errorf("%w: closed Item response has no state", errTrackerEvidenceInvalid)
	default:
		return fmt.Errorf("%w: closed Item response has invalid state", errTrackerEvidenceInvalid)
	}
}

// SetTags adds and removes labels on one item in one call.
func (t githubTracker) SetTags(id string, add, remove []string) error {
	args := []string{"issue", "edit", "-R", t.repo, id}
	for _, label := range add {
		args = append(args, "--add-label", label)
	}
	for _, label := range remove {
		args = append(args, "--remove-label", label)
	}
	_, err := ghJSON(args...)
	if err != nil {
		return fmt.Errorf("%w: set Item tags: %v", errTrackerUnavailable, err)
	}
	return nil
}

// issueJSON is the raw GitHub CLI issue shape. It only exists to feed the
// adapter; the controller reads Item.
type issueJSON struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	UpdatedAt string    `json:"updatedAt"`
	Comments  []comment `json:"comments"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// asItem converts one GitHub issue into the opaque Item the controller reads.
// The issue number becomes a string id so the controller never parses it; a
// second work source feeds Item directly and never needs this converter.
func (i issueJSON) asItem() Item {
	item := Item{
		ID:        strconv.Itoa(i.Number),
		Title:     i.Title,
		Body:      i.Body,
		UpdatedAt: i.UpdatedAt,
		Comments:  i.Comments,
	}
	for _, label := range i.Labels {
		item.Tags = append(item.Tags, label.Name)
	}
	return item
}

// trackerFor builds the work source for a repo. Flows ask for a Tracker here
// instead of a GitHub-shaped type, so a non-GitHub source swaps only this one
// constructor. Tests replace the var to drive the controller with a fake.
var trackerFor = func(repo string) Tracker { return githubTracker{repo: repo} }

var (
	errTrackerUnavailable           = errors.New("Tracker is unavailable")
	errTrackerEvidenceInvalid       = errors.New("Tracker evidence is invalid")
	errTrackerEffectNotApplied      = errors.New("Tracker Effect was not applied")
	errTrackerItemIDEmpty           = errors.New("Tracker Item ID is empty")
	errTrackerItemIDCredential      = errors.New("Tracker Item ID is credential-shaped")
	errTrackerItemIDMismatch        = errors.New("Tracker Item ID does not match requested identity")
	errTrackerRevisionEmpty         = errors.New("Tracker Item Revision is empty")
	errTrackerRevisionCredential    = errors.New("Tracker Item Revision is credential-shaped")
	errTrackerItemIdentityDuplicate = errors.New("Tracker Item identity is duplicated")
)

func validateTrackerItemID(id string) error {
	if id == "" {
		return errTrackerItemIDEmpty
	}
	if secretShaped(id) {
		return errTrackerItemIDCredential
	}
	return nil
}

func validateTrackerItem(item Item, expectedID string) error {
	if err := validateTrackerItemID(item.ID); err != nil {
		return err
	}
	if expectedID != "" && item.ID != expectedID {
		return errTrackerItemIDMismatch
	}
	if strings.TrimSpace(item.UpdatedAt) == "" {
		return errTrackerRevisionEmpty
	}
	if secretShaped(item.UpdatedAt) {
		return errTrackerRevisionCredential
	}
	return nil
}

type trackerSnapshot struct {
	Items    []Item
	Failures []Subject
}

func readTrackerSnapshot(t Tracker) (trackerSnapshot, error) {
	items, err := t.ListOpen()
	if err != nil {
		return trackerSnapshot{}, err
	}
	counts := make(map[string]int, len(items))
	for _, item := range items {
		counts[item.ID]++
	}
	snapshot := trackerSnapshot{Items: make([]Item, 0, len(items))}
	for i, item := range items {
		cause := validateTrackerItem(item, "")
		if counts[item.ID] > 1 {
			cause = errors.Join(cause, errTrackerItemIdentityDuplicate)
		}
		if cause == nil {
			snapshot.Items = append(snapshot.Items, item)
			continue
		}
		material, _ := json.Marshal(item)
		revision := blobSHA(strconv.Itoa(i) + "\x00" + string(material))
		subject := Subject{
			Key:      "tracker-evidence-" + revision,
			Kind:     subjectItem,
			Revision: revision,
			Label:    "invalid Tracker evidence",
			Failure:  fmt.Errorf("%w: %w", errTrackerEvidenceInvalid, cause),
		}
		if validateTrackerItemID(item.ID) == nil && counts[item.ID] == 1 {
			subject.ID = item.ID
		}
		snapshot.Failures = append(snapshot.Failures, subject)
	}
	return snapshot, nil
}

func validatedTrackerItem(t Tracker, id string) (Item, error) {
	if err := validateTrackerItemID(id); err != nil {
		return Item{}, fmt.Errorf("%w: requested Item identity: %w",
			errTrackerEvidenceInvalid, err)
	}
	item, err := t.Get(id)
	if err != nil {
		return Item{}, err
	}
	if err := validateTrackerItem(item, id); err != nil {
		return Item{}, fmt.Errorf("%w: %w", errTrackerEvidenceInvalid, err)
	}
	return item, nil
}
func hasCommentMarker(comments []comment, marker string) bool {
	for _, c := range comments {
		if strings.Contains(c.Body, marker) {
			return true
		}
	}
	return false
}

func publishTrackerComment(
	repoDir string,
	t Tracker,
	it Item,
	kind, revision, body, marker string,
) error {
	claim, err := readAttempts(repoDir, effectAttemptKey(kind, it.ID, revision))
	if err != nil {
		return err
	}
	current, err := validatedTrackerItem(t, it.ID)
	if err != nil {
		return err
	}
	if hasCommentMarker(current.Comments, marker) {
		return nil
	}
	if claim != 0 {
		return fmt.Errorf("%w: prior Tracker comment attempt for Item %q is not visible",
			errHostMergeUnavailable, it.ID)
	}
	if err := claimEffect(repoDir, kind, it.ID, revision); err != nil {
		return err
	}
	commentBody := body
	if marker != "" && !strings.Contains(body, marker) {
		commentBody += "\n\n" + marker
	}
	if err := t.Comment(it.ID, commentBody); err != nil {
		current, readErr := validatedTrackerItem(t, it.ID)
		if readErr != nil {
			if errors.Is(readErr, errTrackerEvidenceInvalid) {
				return readErr
			}
			return fmt.Errorf("%w: comment: %v; reconcile: %v",
				errHostMergeUnavailable, err, readErr)
		}
		if hasCommentMarker(current.Comments, marker) {
			return nil
		}
		return fmt.Errorf("%w: publish Tracker comment: %v",
			errHostMergeUnavailable, err)
	}
	return nil
}

// hasTag reports whether an item carries one tag.
func (it Item) hasTag(name string) bool {
	for _, tag := range it.Tags {
		if tag == name {
			return true
		}
	}
	return false
}

func eligibleItemsAndFailures(cfg Config, repoDir string) ([]Item, []Subject, error) {
	snapshot, err := readTrackerSnapshot(trackerFor(cfg.Repo))
	if err != nil {
		return nil, nil, err
	}
	branchSnapshot, err := readForestBranchSnapshot(repoDir)
	if err != nil {
		return nil, nil, err
	}
	failures := append(snapshot.Failures, branchSnapshot.Failures...)
	if len(snapshot.Items) == 0 {
		return nil, failures, nil
	}
	retiring, err := retirementItemIDs(repoDir)
	if err != nil {
		return nil, nil, err
	}
	items := eligibleFrom(snapshot.Items, branchSnapshot.Covered, retiring,
		cfg.Flows.Builder.ExcludeLabels, cfg.Flows.Builder.RequireLabels)
	return items, failures, nil
}

func eligibleItems(cfg Config, repoDir string) ([]Item, error) {
	items, _, err := eligibleItemsAndFailures(cfg, repoDir)
	return items, err
}

func eligibleFrom(items []Item, branches, retiring, excluded, required []string) []Item {
	covered := make(map[string]bool, len(branches)+len(retiring))
	for _, branch := range branches {
		covered[itemIDFromBranch(branch)] = true
	}
	for _, id := range retiring {
		covered[id] = true
	}
	var ready []Item
	for _, item := range items {
		if covered[item.ID] {
			continue
		}
		// Declared required labels turn selection into an opt-in: a promoter's
		// label earns an item its turn. The two label filters compose, so an
		// item stays eligible only when it also carries no excluded label.
		if len(required) > 0 && !hasTags(item, required) {
			continue
		}
		if item.hasTag(failedLabel) || hasExcludedLabel(item, excluded) {
			continue
		}
		ready = append(ready, item)
	}
	return ready
}

func hasTags(item Item, required []string) bool {
	for _, tag := range required {
		if !item.hasTag(tag) {
			return false
		}
	}
	return true
}

func hasExcludedLabel(item Item, excluded []string) bool {
	for _, tag := range excluded {
		if item.hasTag(tag) {
			return true
		}
	}
	return false
}

// A branch snapshot keeps safe work moving without treating damaged evidence as
// absent. Covered includes every ref with a recoverable Item identity, while
// Actionable contains only unique canonical branches.
type forestBranchSnapshot struct {
	Covered    []string
	Actionable []string
	Failures   []Subject
}

type forestBranchEvidence struct {
	raw      string
	revision string
	branch   string
	id       string
	cause    error
}

func readForestBranchSnapshot(repoDir string) (forestBranchSnapshot, error) {
	out, err := gitCommand(repoDir, "ls-remote", "origin", "refs/heads/forest/*")
	if err != nil {
		return forestBranchSnapshot{}, err
	}
	if strings.TrimSpace(out) == "" {
		return forestBranchSnapshot{}, nil
	}
	var entries []forestBranchEvidence
	counts := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		entry := forestBranchEvidence{raw: line, revision: blobSHA(line)}
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], "refs/heads/"+BranchPrefix) {
			entry.cause = errors.New("forest branch listing has invalid remote evidence")
			entries = append(entries, entry)
			continue
		}
		entry.branch = strings.TrimPrefix(fields[1], "refs/heads/")
		if validHex(fields[0], 20) {
			entry.revision = fields[0]
		} else {
			entry.cause = errors.New("forest branch has an invalid Revision")
		}
		name := strings.TrimPrefix(entry.branch, BranchPrefix)
		delimiter := strings.IndexByte(name, '-')
		segment := ""
		if delimiter >= 1 {
			segment = name[:delimiter]
		} else if delimiter == -1 {
			// A missing slug is malformed, but the full segment still identifies
			// the Item whose duplicate Builder work must remain blocked.
			segment = name
		}
		candidate := decodeBranchID(segment)
		if validateTrackerItemID(candidate) == nil && encodeBranchID(candidate) == segment {
			entry.id = candidate
		}
		if entry.id == "" || delimiter < 1 ||
			encodeBranchID(entry.id) != segment ||
			secretShaped(entry.branch) {
			entry.cause = errors.Join(entry.cause,
				errors.New("forest branch has invalid Tracker Item identity"))
		}
		if entry.id != "" {
			counts[entry.id]++
		}
		entries = append(entries, entry)
	}
	snapshot := forestBranchSnapshot{}
	for _, entry := range entries {
		if entry.id != "" {
			snapshot.Covered = append(snapshot.Covered, entry.branch)
		}
		if entry.cause == nil && counts[entry.id] == 1 {
			snapshot.Actionable = append(snapshot.Actionable, entry.branch)
			continue
		}
		if entry.cause == nil {
			entry.cause = fmt.Errorf("multiple forest branches claim Tracker Item %q", entry.id)
		}
		subject := Subject{
			Key:      "branch-evidence-" + blobSHA(entry.raw),
			Kind:     subjectBranch,
			Revision: entry.revision,
			Label:    "invalid forest branch evidence",
			Failure:  fmt.Errorf("%w: %w", errTrackerEvidenceInvalid, entry.cause),
		}
		if !secretShaped(entry.branch) {
			subject.Branch = entry.branch
		}
		if entry.id != "" {
			subject.ID = entry.id
			subject.Item = Item{ID: entry.id}
		}
		snapshot.Failures = append(snapshot.Failures, subject)
	}
	return snapshot, nil
}

func forestBranches(repoDir string) ([]string, error) {
	snapshot, err := readForestBranchSnapshot(repoDir)
	if err != nil {
		return nil, err
	}
	if len(snapshot.Failures) != 0 {
		return nil, snapshot.Failures[0].Failure
	}
	return snapshot.Actionable, nil
}

func requalifyForestBranch(repoDir, branch string) error {
	snapshot, err := readForestBranchSnapshot(repoDir)
	if err != nil {
		return err
	}
	for _, current := range snapshot.Actionable {
		if current == branch {
			return nil
		}
	}
	for _, failure := range snapshot.Failures {
		if failure.Branch == branch {
			return failure.Failure
		}
	}
	return fmt.Errorf("%w: branch %q is no longer a canonical remote branch",
		errSubjectRevisionStale, branch)
}

func lookupBranchHead(repoDir, branch string) (string, bool, error) {
	ref := branch
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	out, err := gitCommand(repoDir, "ls-remote", "origin", ref)
	if err != nil {
		return "", false, err
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", false, nil
	}
	if len(fields) != 2 || !validHex(fields[0], 20) || fields[1] != ref {
		return "", false, errors.New("branch lookup has invalid remote evidence")
	}
	return fields[0], true, nil
}

func branchHead(repoDir, branch string) (string, error) {
	head, found, err := lookupBranchHead(repoDir, branch)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("branch %s not found", branch)
	}
	return head, nil
}

func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_' || r == '.':
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 32 {
		out = out[:32]
	}
	if out == "" {
		out = "item"
	}
	return strings.Trim(out, "-")
}
