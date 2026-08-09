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
		"--json", "number,title,body,updatedAt,comments,labels", "--limit", "200")
	if err != nil {
		return nil, err
	}
	var raw []issueJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(raw))
	for _, it := range raw {
		items = append(items, it.asItem())
	}
	return items, nil
}

// Get reads one item by its opaque id.
func (t githubTracker) Get(id string) (Item, error) {
	out, err := ghJSON("issue", "view", id, "-R", t.repo,
		"--json", "number,title,body,updatedAt,comments,labels")
	if err != nil {
		return Item{}, err
	}
	var raw issueJSON
	if err := json.Unmarshal(out, &raw); err != nil {
		return Item{}, err
	}
	return raw.asItem(), nil
}

// Comment appends a comment to an item's discussion.
func (t githubTracker) Comment(id, body string) error {
	_, err := ghJSON("issue", "comment", "-R", t.repo, id, "--body", body)
	return err
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
		return closeErr
	}
	var state struct {
		State string `json:"state"`
	}
	if json.Unmarshal(out, &state) == nil && strings.EqualFold(state.State, "closed") {
		return nil
	}
	return closeErr
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
	if len(args) == 5 {
		return nil
	}
	_, err := ghJSON(args...)
	return err
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
	errTrackerItemIDEmpty        = errors.New("Tracker Item ID is empty")
	errTrackerItemIDCredential   = errors.New("Tracker Item ID is credential-shaped")
	errTrackerItemIDMismatch     = errors.New("Tracker Item ID does not match requested identity")
	errTrackerRevisionCredential = errors.New("Tracker Item Revision is credential-shaped")
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
	if secretShaped(item.UpdatedAt) {
		return errTrackerRevisionCredential
	}
	return nil
}

func validatedTrackerItems(t Tracker) ([]Item, error) {
	items, err := t.ListOpen()
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if err := validateTrackerItem(item, ""); err != nil {
			return nil, err
		}
	}
	return items, nil
}

func validatedTrackerItem(t Tracker, id string) (Item, error) {
	if err := validateTrackerItemID(id); err != nil {
		return Item{}, err
	}
	item, err := t.Get(id)
	if err != nil {
		return Item{}, err
	}
	if err := validateTrackerItem(item, id); err != nil {
		return Item{}, err
	}
	return item, nil
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

func eligibleItems(cfg Config, repoDir string) ([]Item, error) {
	items, err := validatedTrackerItems(trackerFor(cfg.Repo))
	if err != nil {
		return nil, err
	}
	branches, err := forestBranches(repoDir)
	if err != nil {
		return nil, err
	}
	retiring, err := retirementItemIDs(repoDir)
	if err != nil {
		return nil, err
	}
	return eligibleFrom(items, branches, retiring,
		cfg.Flows.Builder.ExcludeLabels, cfg.Flows.Builder.RequireLabels), nil
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
		if hasExcludedLabel(item, excluded) {
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

func forestBranches(repoDir string) ([]string, error) {
	out, err := gitCommand(repoDir, "ls-remote", "origin", "refs/heads/forest/*")
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[1], "refs/heads/") {
			continue
		}
		branch := strings.TrimPrefix(fields[1], "refs/heads/")
		if secretShaped(branch) || validateTrackerItemID(itemIDFromBranch(branch)) != nil {
			return nil, errors.New("forest branch has invalid Tracker Item identity")
		}
		branches = append(branches, branch)
	}
	return branches, nil
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
