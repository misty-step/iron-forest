package main

import (
	"errors"
	"sync"
	"testing"
)

// TestClaimRefusedPinsTheGates pins the refusal predicate behind the claim: an
// issue that is already claimed, failed, parked, or covered by an open pull
// request must not be claimable again. This is the durable check that turns the
// list-then-claim race into a re-check-then-claim.
func TestClaimRefusedPinsTheGates(t *testing.T) {
	plain := func() issue { return issue{Number: 7} }
	with := func(name string) issue {
		it := plain()
		it.Labels = []struct {
			Name string `json:"name"`
		}{{Name: name}}
		return it
	}

	cases := []struct {
		name string
		it   issue
		hits []string
		want bool
	}{
		{"open and uncovered", plain(), nil, false},
		{"claimed", with(claimLabel), nil, true},
		{"failed", with(failedLabel), nil, true},
		{"parked", with("parked"), nil, true},
		{"covered by open PR", plain(), []string{"#1 fix"}, true},
	}
	for _, c := range cases {
		if got := claimRefused(c.it, c.hits); got != c.want {
			t.Errorf("%s: claimRefused = %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeClaimStore is an in-memory claimStore whose claimRef is a genuine
// compare-and-set under a mutex, mirroring the repository-visible git-ref lease
// the production store uses. It lets tests drive the real claimFrom path — the
// concurrent claim, the refusal gates, and release-on-failure — offline.
type fakeClaimStore struct {
	mu       sync.Mutex
	claims   map[int]bool
	labels   map[int][]string
	prList   map[int][]string
	issueErr error
	setErr   error
}

func newFakeClaimStore() *fakeClaimStore {
	return &fakeClaimStore{
		claims: make(map[int]bool),
		labels: make(map[int][]string),
		prList: make(map[int][]string),
	}
}

func (f *fakeClaimStore) claimRef(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.claims[n] {
		return errAlreadyClaimed
	}
	f.claims[n] = true
	return nil
}

func (f *fakeClaimStore) issue(n int) (issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.issueErr != nil {
		return issue{}, f.issueErr
	}
	it := issue{Number: n}
	for _, l := range f.labels[n] {
		it.Labels = append(it.Labels, struct {
			Name string `json:"name"`
		}{Name: l})
	}
	return it, nil
}

func (f *fakeClaimStore) openPRs(n int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prList[n], nil
}

func (f *fakeClaimStore) addClaimLabel(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	f.labels[n] = append(f.labels[n], claimLabel)
	return nil
}

func (f *fakeClaimStore) deleteClaimRef(n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.claims, n)
	return nil
}

// TestClaimFromExactlyOneWins is the cross-host oracle for the concurrent
// claim: two workers claiming the same item must resolve to exactly one winner
// and the loser is refused with errAlreadyClaimed. This exercises the real
// compare-and-set claim path, not just the in-process broker.
func TestClaimFromExactlyOneWins(t *testing.T) {
	store := newFakeClaimStore()
	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = claimFrom(store, 7)
		}(i)
	}
	wg.Wait()

	winners := 0
	for i, err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, errAlreadyClaimed):
			// expected: the losing worker is refused
		default:
			t.Fatalf("pass %d: unexpected error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one worker must win the claim; got %d winners", winners)
	}
	if !store.claims[7] {
		t.Fatal("the claim ref must be held after a successful claim")
	}
}

// TestConcurrentPassesClaimDisjointItems is the contention oracle: two
// concurrent passes over the same backlog must claim disjoint items, and every
// item must end up claimed by exactly one pass — proven on the real claimFrom
// path with genuinely concurrent, conflicting compare-and-sets.
func TestConcurrentPassesClaimDisjointItems(t *testing.T) {
	const items = 64
	store := newFakeClaimStore()
	won := make([][]int, 2)
	var wonMu sync.Mutex
	var wg sync.WaitGroup
	for pass := 0; pass < 2; pass++ {
		wg.Add(1)
		go func(pass int) {
			defer wg.Done()
			for id := 1; id <= items; id++ {
				err := claimFrom(store, id)
				switch {
				case err == nil:
					wonMu.Lock()
					won[pass] = append(won[pass], id)
					wonMu.Unlock()
				case errors.Is(err, errAlreadyClaimed):
					// the other pass won this one
				default:
					t.Errorf("pass %d item %d: unexpected error: %v", pass, id, err)
				}
			}
		}(pass)
	}
	wg.Wait()

	seen := make(map[int]int, items)
	for _, grp := range won {
		for _, id := range grp {
			seen[id]++
		}
	}
	for id, c := range seen {
		if c > 1 {
			t.Fatalf("item %d claimed by %d passes", id, c)
		}
	}
	if len(seen) != items {
		t.Fatalf("claimed %d items, want all %d", len(seen), items)
	}
}

// TestFailedPreLabelReadReleasesClaim pins the reviewer's release requirement
// for a claim that dies on the pre-label issue read: the claim ref is undone so
// the item stays claimable and retryable, never unclaimed-but-stuck.
func TestFailedPreLabelReadReleasesClaim(t *testing.T) {
	store := newFakeClaimStore()
	store.issueErr = errors.New("github read flaked")

	if err := claimFrom(store, 7); err == nil {
		t.Fatal("claim must fail when the pre-label read fails")
	}
	if store.claims[7] {
		t.Fatal("the claim ref must be released after a failed pre-label read")
	}

	store.issueErr = nil
	if err := claimFrom(store, 7); err != nil {
		t.Fatalf("the item must be claimable again after release: %v", err)
	}
}

// TestFailedLabelWriteReleasesClaim pins the release requirement for a claim
// that dies on the durable label write: the ref is undone and the item becomes
// claimable again, so it is never unclaimed-but-unretryable for the daemon.
func TestFailedLabelWriteReleasesClaim(t *testing.T) {
	store := newFakeClaimStore()
	store.setErr = errors.New("label write flaked")

	if err := claimFrom(store, 7); err == nil {
		t.Fatal("claim must fail when the label write fails")
	}
	if store.claims[7] {
		t.Fatal("the claim ref must be released after a failed label write")
	}

	store.setErr = nil
	if err := claimFrom(store, 7); err != nil {
		t.Fatalf("the item must be claimable again after release: %v", err)
	}
}

// TestClaimBrokerReleaseFreesItem covers the in-process broker release that
// keeps a failed mid-claim item retryable inside the daemon.
func TestClaimBrokerReleaseFreesItem(t *testing.T) {
	b := newClaimBroker()
	if !b.acquire(7) {
		t.Fatal("first acquirer must be granted the item")
	}
	b.release(7)
	if !b.acquire(7) {
		t.Fatal("after release the same item must be acquirable again")
	}
}

// TestClaimIssueReportsAlreadyClaimed pins the sentinel chewLoop switches on. A
// refusal must be recognisable with errors.Is, because the alternative is what
// happened on 2026-08-05: a leaked claim ref made issue #92 permanently
// unclaimable, each pass recorded it as a failed run, and since it stayed first
// in the backlog it starved the eight cards queued behind it for a quarter hour.
func TestClaimIssueReportsAlreadyClaimed(t *testing.T) {
	const item = 4242
	if !processClaims.acquire(item) {
		t.Fatal("a fresh item must be acquirable")
	}
	defer processClaims.release(item)

	// The broker refuses before any repository call, so this stays offline.
	err := claimIssue("owner/repo", item)
	if !errors.Is(err, errAlreadyClaimed) {
		t.Fatalf("claimIssue = %v, want an errAlreadyClaimed sentinel", err)
	}
}
