package main

import "fmt"

// memoryTracker is an in-memory tracker used by tests. Open items live in the
// map; closing one removes it, exactly as a host would stop returning it.
type memoryTracker struct {
	items map[string]Item
}

// newMemoryTracker returns an empty in-memory tracker.
func newMemoryTracker() *memoryTracker {
	return &memoryTracker{items: make(map[string]Item)}
}

// seed inserts or replaces one item by id.
func (m *memoryTracker) seed(it Item) {
	m.items[it.ID] = it
}

// ListOpen implements Tracker.
func (m *memoryTracker) ListOpen() ([]Item, error) {
	items := make([]Item, 0, len(m.items))
	for _, it := range m.items {
		items = append(items, it)
	}
	return items, nil
}

// Get implements Tracker.
func (m *memoryTracker) Get(id string) (Item, error) {
	it, ok := m.items[id]
	if !ok {
		return Item{}, fmt.Errorf("item %q not found", id)
	}
	return it, nil
}

// Comment implements Tracker.
func (m *memoryTracker) Comment(id, body string) error {
	it, err := m.Get(id)
	if err != nil {
		return err
	}
	it.Comments = append(it.Comments, comment{Body: body})
	m.items[id] = it
	return nil
}

// Close implements Tracker. It is idempotent: closing an item that is already
// closed (no longer in the map) is a no-op that returns nil, matching the Tracker
// contract so a retry after a crash is safe.
func (m *memoryTracker) Close(id string) error {
	delete(m.items, id)
	return nil
}

// SetTags implements Tracker.
func (m *memoryTracker) SetTags(id string, add, remove []string) error {
	it, err := m.Get(id)
	if err != nil {
		return err
	}
	tags := make(map[string]bool, len(it.Tags)+len(add))
	for _, t := range it.Tags {
		tags[t] = true
	}
	for _, t := range remove {
		delete(tags, t)
	}
	for _, t := range add {
		tags[t] = true
	}
	it.Tags = it.Tags[:0]
	for t := range tags {
		it.Tags = append(it.Tags, t)
	}
	m.items[id] = it
	return nil
}

// memoryProjection is an in-memory projection used by tests. One open
// projection is keyed by its branch; merging one removes it.
type memoryProjection struct {
	branches map[string]Item
	next     int
}

// newMemoryProjection returns an empty in-memory projection.
func newMemoryProjection() *memoryProjection {
	return &memoryProjection{branches: make(map[string]Item), next: 1}
}

// FindOpen implements Projection.
func (m *memoryProjection) FindOpen(branch string) (Item, error) {
	pr, ok := m.branches[branch]
	if !ok {
		return Item{}, fmt.Errorf("no open projection for branch %q", branch)
	}
	return pr, nil
}

// Open implements Projection.
func (m *memoryProjection) Open(branch, title, body string) (Item, error) {
	if _, ok := m.branches[branch]; ok {
		return Item{}, fmt.Errorf("projection already open for branch %q", branch)
	}
	pr := Item{ID: fmt.Sprintf("%d", m.next), Title: title, Body: body}
	m.next++
	m.branches[branch] = pr
	return pr, nil
}

// Comment implements Projection.
func (m *memoryProjection) Comment(id, body string) error {
	for branch, pr := range m.branches {
		if pr.ID == id {
			pr.Comments = append(pr.Comments, comment{Body: body})
			m.branches[branch] = pr
			return nil
		}
	}
	return fmt.Errorf("projection %q not found", id)
}

// Merge implements Projection.
func (m *memoryProjection) Merge(id, strategy string) error {
	for branch, pr := range m.branches {
		if pr.ID == id {
			delete(m.branches, branch)
			return nil
		}
	}
	return fmt.Errorf("projection %q not found", id)
}
