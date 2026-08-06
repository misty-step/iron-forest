package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// issue is the unit of work. One open GitHub issue becomes one pull request.
type issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

func (it issue) hasLabel(name string) bool {
	for _, l := range it.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

func ghJSON(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	return cmd.Output()
}

func listOpenIssues(repo string) ([]issue, error) {
	out, err := ghJSON("issue", "list", "-R", repo, "--state", "open",
		"--json", "number,title,body,labels", "--limit", "200")
	if err != nil {
		return nil, err
	}
	var items []issue
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func getIssue(repo string, n int) (issue, error) {
	it, err := listOpenIssues(repo)
	if err != nil {
		return issue{}, err
	}
	for _, i := range it {
		if i.Number == n {
			return i, nil
		}
	}
	return issue{}, fmt.Errorf("issue %d is not open", n)
}

// getIssueAny fetches an issue in any state (the reaction loop re-enters
// issues that are already closed because their PR is open).
func getIssueAny(repo string, n int) (issue, error) {
	out, err := ghJSON("issue", "view", fmt.Sprintf("%d", n), "-R", repo,
		"--json", "number,title,body,labels")
	if err != nil {
		return issue{}, err
	}
	var it issue
	if err := json.Unmarshal(out, &it); err != nil {
		return issue{}, err
	}
	return it, nil
}

// openPRsReferencing finds whether an open PR mentions the issue.
func openPRsReferencing(repo string, n int) ([]string, error) {
	out, err := ghJSON("pr", "list", "-R", repo, "--state", "open",
		"--json", "number,title,body,url,headRefName", "--limit", "200")
	if err != nil {
		return nil, err
	}
	var prs []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		URL         string `json:"url"`
		HeadRefName string `json:"headRefName"`
	}
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("#%d", n)
	var hits []string
	for _, p := range prs {
		if strings.Contains(p.Body, ref) || strings.Contains(p.Title, ref) {
			hits = append(hits, fmt.Sprintf("#%d %s %s", p.Number, p.Title, p.URL))
		}
	}
	return hits, nil
}

// backlog is every open issue this loop may chew: open, unclaimed by forest,
// not failed by forest, and not already covered by an open pull request.
func backlog(cfg Config) ([]issue, error) {
	items, err := listOpenIssues(cfg.Repo)
	if err != nil {
		return nil, err
	}
	var ready []issue
	for _, it := range items {
		if it.hasLabel(claimLabel) || it.hasLabel(failedLabel) || it.hasLabel("parked") {
			continue
		}
		hits, err := openPRsReferencing(cfg.Repo, it.Number)
		if err != nil {
			return nil, err
		}
		if len(hits) > 0 {
			continue
		}
		ready = append(ready, it)
	}
	return ready, nil
}

func ensureLabels(repo string) error {
	for _, label := range []string{claimLabel, failedLabel} {
		// --force keeps the call idempotent when the label already exists.
		_, _ = ghJSON("label", "create", "-R", repo, label, "--color", "a371f7", "--force")
	}
	return nil
}

// claimBroker is the in-process referee for work-item ownership. It hands each
// issue to exactly one acquiring worker, so two concurrent passes over the same
// backlog claim disjoint items: the first acquirer wins and every later one is
// refused, even before the durable claim on GitHub has propagated.
type claimBroker struct {
	mu      sync.Mutex
	claimed map[int]bool
}

func newClaimBroker() *claimBroker {
	return &claimBroker{claimed: make(map[int]bool)}
}

// acquire grants n to the caller if nobody else in this process holds it yet.
// It returns false — the refusal — when another pass already claimed n.
func (b *claimBroker) acquire(n int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.claimed[n] {
		return false
	}
	b.claimed[n] = true
	return true
}

// release returns n to the broker so a later pass can try again. It is called
// whenever a claim fails after the broker granted it, so a mid-claim failure —
// a failed pre-label read or label write — never leaves the item blocked for
// the lifetime of the daemon.
func (b *claimBroker) release(n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.claimed, n)
}

// processClaims is the claimBroker shared by every claim this binary makes, so
// the running daemon never double-claims an item across its own passes.
var processClaims = newClaimBroker()

// claimRefPrefix is the git ref under which a claim is recorded. Creating the
// ref is a compare-and-set on GitHub (it fails if the ref already exists), so
// it is the repository-visible atomic lease that closes the cross-host race.
const claimRefPrefix = "refs/heads/forest/claim/issue-"

// errAlreadyClaimed is returned by a claimStore.claimRef when the claim ref
// already exists: some other worker, on any host, already owns the item.
var errAlreadyClaimed = errors.New("claim ref already exists")

// claimStore is the set of repository mutations a claim needs. Production uses
// ghClaimStore backed by the GitHub CLI; tests drive the real concurrent claim
// path with an in-memory fake that performs the same compare-and-set.
type claimStore interface {
	// claimRef atomically creates the claim ref iff it does not exist. It
	// returns errAlreadyClaimed when another worker already owns the ref.
	claimRef(n int) error
	// issue fetches the issue's current state (labels).
	issue(n int) (issue, error)
	// openPRs lists open pull requests that reference the issue.
	openPRs(n int) ([]string, error)
	// addClaimLabel applies the durable claim label.
	addClaimLabel(n int) error
	// deleteClaimRef undoes a claim won by this worker.
	deleteClaimRef(n int) error
}

// ghClaimStore is the claimStore backed by the GitHub CLI.
type ghClaimStore struct {
	repo string
}

// defaultBranch returns the repository's default branch name.
func defaultBranch(repo string) (string, error) {
	out, err := ghJSON("api", fmt.Sprintf("repos/%s", repo))
	if err != nil {
		return "", err
	}
	var r struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", err
	}
	return r.DefaultBranch, nil
}

// defaultBranchSHA returns the tip commit sha of the default branch, the
// object a new claim ref is pointed at.
func defaultBranchSHA(repo string) (string, error) {
	branch, err := defaultBranch(repo)
	if err != nil {
		return "", err
	}
	out, err := ghJSON("api", fmt.Sprintf("repos/%s/git/ref/heads/%s", repo, branch))
	if err != nil {
		return "", err
	}
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return "", err
	}
	return r.Object.SHA, nil
}

// claimRef creates the claim ref for n, succeeding only if the ref does not
// yet exist. A 422 from GitHub ("Reference already exists") means another host
// already claimed the item, reported as errAlreadyClaimed.
func (s ghClaimStore) claimRef(n int) error {
	sha, err := defaultBranchSHA(s.repo)
	if err != nil {
		return err
	}
	_, err = ghJSON("api", "--method", "POST",
		fmt.Sprintf("repos/%s/git/refs", s.repo),
		"-f", fmt.Sprintf("ref=%s%d", claimRefPrefix, n),
		"-f", "sha="+sha)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok &&
			strings.Contains(string(ee.Stderr), "already exists") {
			return errAlreadyClaimed
		}
		return err
	}
	return nil
}

// issue fetches the issue's current labels for the refusal re-check.
func (s ghClaimStore) issue(n int) (issue, error) {
	return getIssueAny(s.repo, n)
}

// openPRs lists open pull requests referencing the issue.
func (s ghClaimStore) openPRs(n int) ([]string, error) {
	return openPRsReferencing(s.repo, n)
}

// addClaimLabel writes the durable claim label.
func (s ghClaimStore) addClaimLabel(n int) error {
	_, err := ghJSON("issue", "edit", "-R", s.repo, fmt.Sprintf("%d", n),
		"--add-label", claimLabel)
	return err
}

// deleteClaimRef removes the claim ref won by this worker.
func (s ghClaimStore) deleteClaimRef(n int) error {
	_, err := ghJSON("api", "--method", "DELETE",
		fmt.Sprintf("repos/%s/git/refs/heads/forest/claim/issue-%d", s.repo, n))
	return err
}

// wipIssues lists every issue carrying the claim label, in any state, so a
// reconcile pass can decide which of them still represent live work.
func (s ghClaimStore) wipIssues() ([]issue, error) {
	out, err := ghJSON("issue", "list", "-R", s.repo, "--state", "all",
		"--label", claimLabel, "--json", "number,title,body,labels", "--limit", "200")
	if err != nil {
		return nil, err
	}
	var items []issue
	if err := json.Unmarshal(out, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// hasClaimRef reports whether the item still holds its claim ref, the durable
// cross-host lease that marks a genuinely live claim. A missing ref (HTTP 404)
// means the work is not in flight, so the label is stale and may be cleared.
func (s ghClaimStore) hasClaimRef(n int) (bool, error) {
	_, err := ghJSON("api", fmt.Sprintf("repos/%s/git/ref/heads/forest/claim/issue-%d", s.repo, n))
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok &&
			strings.Contains(string(ee.Stderr), "404") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// removeClaimLabel clears the durable claim label from an item.
func (s ghClaimStore) removeClaimLabel(n int) error {
	_, err := ghJSON("issue", "edit", "-R", s.repo, fmt.Sprintf("%d", n),
		"--remove-label", claimLabel)
	return err
}

// claimRefused reports whether an issue can no longer be claimed: it is already
// claimed, failed, or parked, or an open pull request already covers it.
func claimRefused(it issue, hits []string) bool {
	if it.hasLabel(claimLabel) || it.hasLabel(failedLabel) || it.hasLabel("parked") {
		return true
	}
	return len(hits) > 0
}

// claimFrom is the core of claimIssue against an injected claimStore. It first
// wins the repository-visible compare-and-set ref, so across every host exactly
// one worker owns the item; it then re-reads the issue so the durable label
// write is never based on a stale listing and refuses an item that is already
// claimed, failed, parked, or covered by an open pull request. Any failure
// after winning the ref undoes it, so the item stays claimable and retryable.
func claimFrom(store claimStore, n int) (err error) {
	if err := store.claimRef(n); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = store.deleteClaimRef(n)
		}
	}()

	it, err := store.issue(n)
	if err != nil {
		return err
	}
	if claimRefused(it, nil) {
		return fmt.Errorf("issue %d is already claimed", n)
	}
	hits, err := store.openPRs(n)
	if err != nil {
		return err
	}
	if len(hits) > 0 {
		return fmt.Errorf("issue %d is already covered by an open pull request", n)
	}
	return store.addClaimLabel(n)
}

// claimIssue claims the item for this forest. The process broker guards one
// in-flight claim per binary; the durable, repository-visible ownership is the
// compare-and-set claim ref in claimFrom, which is atomic across hosts. The
// broker mark is always released on the way out, so a failed claim never blocks
// the item inside this daemon.
func claimIssue(repo string, n int) error {
	if !processClaims.acquire(n) {
		return fmt.Errorf("issue %d: %w", n, errAlreadyClaimed)
	}
	defer processClaims.release(n)
	return claimFrom(ghClaimStore{repo: repo}, n)
}

// releaseClaim drops the claim ref once an item reaches a terminal state. The
// ref is the cross-host mutex for one attempt, not a permanent record: left
// behind, it makes the item unclaimable forever, so a reopened or requeued item
// can never be worked again. Observed on issue #92, whose leaked ref survived a
// failed run and then refused every later attempt.
func releaseClaim(repo string, n int) {
	_ = ghClaimStore{repo: repo}.deleteClaimRef(n)
}

// reconcileStore is the subset of repository access a reconcile pass needs:
// list the items that carry the claim label, ask whether an item still holds
// its claim ref, and remove the claim label.
type reconcileStore interface {
	wipIssues() ([]issue, error)
	hasClaimRef(n int) (bool, error)
	removeClaimLabel(n int) error
}

// reconcileClearsStaleClaims is the reconcile pass that keeps forest:wip
// honest: a claim label must never outlive the work it claimed. It clears the
// label from any item that no longer holds a live claim — one whose issue was
// closed (its ref was released on close) or one that carries the label but
// never started (its ref was never held) — while leaving a genuine in-flight
// claim, which still holds its ref, untouched.
func reconcileClearsStaleClaims(store reconcileStore) error {
	items, err := store.wipIssues()
	if err != nil {
		return err
	}
	for _, it := range items {
		live, err := store.hasClaimRef(it.Number)
		if err != nil {
			return err
		}
		if live {
			continue // a live claim must never be cleared
		}
		if err := store.removeClaimLabel(it.Number); err != nil {
			return err
		}
	}
	return nil
}

// reconcileClaims runs the stale-claim-label pass against the live repository.
func reconcileClaims(repo string) error {
	return reconcileClearsStaleClaims(ghClaimStore{repo: repo})
}

// failIssue records why a run failed, unparks the item, and marks it so the
// loop does not retry it forever.
func failIssue(repo string, n int, msg string) error {
	_, _ = ghJSON("issue", "comment", "-R", repo, fmt.Sprintf("%d", n),
		"--body", "forest run failed: "+msg)
	_, _ = ghJSON("issue", "edit", "-R", repo, fmt.Sprintf("%d", n),
		"--add-label", failedLabel, "--remove-label", claimLabel)
	releaseClaim(repo, n)
	return nil
}

// closeIssue completes the chewed item once its pull request is open. gh has
// no way to close through `issue edit`, so this uses the dedicated command.
func closeIssue(repo string, n int) error {
	_, _ = ghJSON("issue", "edit", "-R", repo, fmt.Sprintf("%d", n),
		"--remove-label", claimLabel, "--remove-label", failedLabel)
	_, err := ghJSON("issue", "close", "-R", repo, fmt.Sprintf("%d", n))
	releaseClaim(repo, n)
	return err
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
