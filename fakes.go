package main

import "fmt"

// memoryTracker is an in-memory tracker used by tests. Open items live in the
// map; closing one removes it, exactly as a host would stop returning it.
type memoryTracker struct {
	items map[string]item
}

// newMemoryTracker returns an empty in-memory tracker.
func newMemoryTracker() *memoryTracker {
	return &memoryTracker{items: make(map[string]item)}
}

// seed inserts or replaces one item by id.
func (m *memoryTracker) seed(it item) {
	m.items[it.ID] = it
}

// ListOpen implements tracker.
func (m *memoryTracker) ListOpen() ([]item, error) {
	items := make([]item, 0, len(m.items))
	for _, it := range m.items {
		items = append(items, it)
	}
	return items, nil
}

// Get implements tracker.
func (m *memoryTracker) Get(id string) (item, error) {
	it, ok := m.items[id]
	if !ok {
		return item{}, fmt.Errorf("item %q not found", id)
	}
	return it, nil
}

// Comment implements tracker.
func (m *memoryTracker) Comment(id, body string) error {
	it, err := m.Get(id)
	if err != nil {
		return err
	}
	it.Comments = append(it.Comments, comment{Body: body})
	m.items[id] = it
	return nil
}

// Close implements tracker.
func (m *memoryTracker) Close(id string) error {
	if _, ok := m.items[id]; !ok {
		return fmt.Errorf("item %q not found", id)
	}
	delete(m.items, id)
	return nil
}

// SetTags implements tracker.
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
	branches map[string]item
	next     int
}

// newMemoryProjection returns an empty in-memory projection.
func newMemoryProjection() *memoryProjection {
	return &memoryProjection{branches: make(map[string]item), next: 1}
}

// FindOpen implements projection.
func (m *memoryProjection) FindOpen(branch string) (item, error) {
	pr, ok := m.branches[branch]
	if !ok {
		return item{}, fmt.Errorf("no open projection for branch %q", branch)
	}
	return pr, nil
}

// Open implements projection.
func (m *memoryProjection) Open(branch, title, body string) (item, error) {
	if _, ok := m.branches[branch]; ok {
		return item{}, fmt.Errorf("projection already open for branch %q", branch)
	}
	pr := item{ID: fmt.Sprintf("%d", m.next), Title: title, Body: body}
	m.next++
	m.branches[branch] = pr
	return pr, nil
}

// Comment implements projection.
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

// Merge implements projection.
func (m *memoryProjection) Merge(id, strategy string) error {
	for branch, pr := range m.branches {
		if pr.ID == id {
			delete(m.branches, branch)
			return nil
		}
	}
	return fmt.Errorf("projection %q not found", id)
}
