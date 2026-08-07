package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryLeaseStore struct {
	mu      sync.Mutex
	refs    map[string]struct{ content, sha string }
	creates int
}

func newMemoryLeaseStore() *memoryLeaseStore {
	return &memoryLeaseStore{refs: make(map[string]struct{ content, sha string })}
}

func (s *memoryLeaseStore) create(ref, content, expectSHA string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if current, ok := s.refs[ref]; ok {
		if expectSHA == "" || current.sha != expectSHA {
			return errLeaseHeld
		}
	}
	s.refs[ref] = struct{ content, sha string }{content: content, sha: blobSHA(content)}
	s.creates++
	return nil
}

func (s *memoryLeaseStore) read(ref string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.refs[ref]
	if !ok {
		return "", "", nil
	}
	return current.sha, current.content, nil
}

func (s *memoryLeaseStore) delete(ref, expectSHA string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.refs[ref]
	if !ok || current.sha != expectSHA {
		return errLeaseHeld
	}
	delete(s.refs, ref)
	return nil
}

func (s *memoryLeaseStore) list(prefix string) ([]leaseRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var refs []leaseRef
	for ref, current := range s.refs {
		if len(ref) >= len(prefix) && ref[:len(prefix)] == prefix {
			refs = append(refs, leaseRef{Ref: ref, SHA: current.sha})
		}
	}
	return refs, nil
}

func staleHolder(t time.Time) string {
	return `{"flow":"Builder","run_id":"old","host":"dead","pid":1,"time":"` +
		t.UTC().Format(time.RFC3339) + `"}`
}

func TestLeaseFirstAcquirerWins(t *testing.T) {
	store := newMemoryLeaseStore()
	cfg := Config{}
	first, err := acquireLeaseFrom(store, cfg, "item-7", "Builder", "run-1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.SHA == "" || first.Ref != "refs/forest/lease/item-7" {
		t.Fatalf("unexpected handle: %+v", first)
	}
	if _, err := acquireLeaseFrom(store, cfg, "item-7", "Fixer", "run-2"); !errors.Is(err, errLeaseHeld) {
		t.Fatalf("second acquire = %v, want lease held", err)
	}
}

func TestLeaseReleaseAllowsLaterAcquirer(t *testing.T) {
	store := newMemoryLeaseStore()
	first, err := acquireLeaseFrom(store, Config{}, "item-8", "Builder", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	releaseLeaseFrom(store, first)
	if _, err := acquireLeaseFrom(store, Config{}, "item-8", "Builder", "run-2"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestLeaseTTLBreaksStaleHolder(t *testing.T) {
	store := newMemoryLeaseStore()
	ref := "refs/forest/lease/item-9"
	old := staleHolder(time.Now().Add(-2 * time.Hour))
	store.refs[ref] = struct{ content, sha string }{content: old, sha: blobSHA(old)}
	cfg := Config{Lease: LeasePolicy{TTLSeconds: 60}}
	if _, err := acquireLeaseFrom(store, cfg, "item-9", "Fixer", "run-new"); err != nil {
		t.Fatalf("stale lease must be replaceable: %v", err)
	}
	if store.creates != 1 {
		t.Fatalf("stale replacement creates = %d, want 1", store.creates)
	}
}

func TestLeaseTTLProtectsFreshHolder(t *testing.T) {
	store := newMemoryLeaseStore()
	ref := "refs/forest/lease/item-10"
	fresh := staleHolder(time.Now())
	store.refs[ref] = struct{ content, sha string }{content: fresh, sha: blobSHA(fresh)}
	cfg := Config{Lease: LeasePolicy{TTLSeconds: 60}}
	if _, err := acquireLeaseFrom(store, cfg, "item-10", "Fixer", "run-new"); !errors.Is(err, errLeaseHeld) {
		t.Fatalf("fresh lease = %v, want lease held", err)
	}
	if store.creates != 0 {
		t.Fatalf("fresh lease must not be replaced")
	}
}

func TestLeaseTTLEdgeProtectsHolder(t *testing.T) {
	store := newMemoryLeaseStore()
	ref := "refs/forest/lease/item-10-edge"
	nearExpiry := staleHolder(time.Now().Add(-59 * time.Second))
	store.refs[ref] = struct{ content, sha string }{content: nearExpiry, sha: blobSHA(nearExpiry)}
	cfg := Config{Lease: LeasePolicy{TTLSeconds: 60}}
	if _, err := acquireLeaseFrom(store, cfg, "item-10-edge", "Fixer", "run-new"); !errors.Is(err, errLeaseHeld) {
		t.Fatalf("near-expiry lease = %v, want lease held", err)
	}
}

func TestLeaseBrokerReportsContentionAndIdle(t *testing.T) {
	broker := newLeaseBroker()
	if !broker.idle() {
		t.Fatal("new broker must be idle")
	}
	if !broker.acquire("branch-forest/7-fix") {
		t.Fatal("first broker acquire must succeed")
	}
	if broker.idle() {
		t.Fatal("broker with an active key must not be idle")
	}
	if broker.acquire("branch-forest/7-fix") {
		t.Fatal("second broker acquire must be refused")
	}
	broker.release("branch-forest/7-fix")
	if !broker.idle() || !broker.acquire("branch-forest/7-fix") {
		t.Fatal("released key must become available")
	}
	broker.release("branch-forest/7-fix")
}

func TestEligibleItemsExcludesLabelsAndCoveredBranches(t *testing.T) {
	labelled := issue{Number: 2}
	labelled.Labels = append(labelled.Labels, struct {
		Name string `json:"name"`
	}{Name: "parked"})
	items := []issue{{Number: 1, Title: "ready"}, labelled, {Number: 3, Title: "covered"}}
	got := eligibleFrom(items, []string{"forest/3-covered"}, []string{"parked"}, nil)
	if len(got) != 1 || got[0].Number != 1 {
		t.Fatalf("eligible items = %+v, want only item 1", got)
	}
}

func TestEligibleItemsRequireLabelsOptIn(t *testing.T) {
	label := func(number int, names ...string) issue {
		it := issue{Number: number}
		for _, name := range names {
			it.Labels = append(it.Labels, struct {
				Name string `json:"name"`
			}{Name: name})
		}
		return it
	}
	unlabelled := label(1)
	promoted := label(2, "forest:ready")
	braked := label(3, "forest:failed", "forest:ready")
	items := []issue{unlabelled, promoted, braked}

	// With require_labels declared, selection is an opt-in: only an open item
	// carrying the required label is selected. ExcludeLabels composes with the
	// opt-in, so an item that also carries the excluded label is not selected.
	got := eligibleFrom(items, nil, []string{"forest:failed"}, []string{"forest:ready"})
	if len(got) != 1 || got[0].Number != 2 {
		t.Fatalf("opt-in eligible items = %+v, want only item 2", got)
	}
}
