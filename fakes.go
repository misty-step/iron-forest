package main

import "fmt"

// memoryTracker is an in-memory tracker used by tests. Open items live in the
// map; closing one moves it to the closed map, exactly as a host would stop
// returning it from ListOpen while still answering a tag query across all states.
type memoryTracker struct {
	items  map[string]Item
	closed map[string]Item
}

// newMemoryTracker returns an empty in-memory tracker.
func newMemoryTracker() *memoryTracker {
	return &memoryTracker{
		items:  make(map[string]Item),
		closed: make(map[string]Item),
	}
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

// ListByTag implements Tracker. It returns every item, open or closed, that
// carries the tag, so the Manager can find a ready assignment on an item closed
// by hand.
func (m *memoryTracker) ListByTag(tag string) ([]Item, error) {
	var out []Item
	for _, it := range m.items {
		if it.hasTag(tag) {
			out = append(out, it)
		}
	}
	for _, it := range m.closed {
		if it.hasTag(tag) {
			out = append(out, it)
		}
	}
	return out, nil
}

// Get implements Tracker.
func (m *memoryTracker) Get(id string) (Item, error) {
	if it, ok := m.items[id]; ok {
		return it, nil
	}
	if it, ok := m.closed[id]; ok {
		return it, nil
	}
	return Item{}, fmt.Errorf("item %q not found", id)
}

// put writes an item back to whichever state table it came from, so a tag edit
// on a closed item never resurrects it into the open list.
func (m *memoryTracker) put(it Item) {
	if _, ok := m.closed[it.ID]; ok {
		m.closed[it.ID] = it
	} else {
		m.items[it.ID] = it
	}
}

// Comment implements Tracker.
func (m *memoryTracker) Comment(id, body string) error {
	it, err := m.Get(id)
	if err != nil {
		return err
	}
	it.Comments = append(it.Comments, comment{Body: body})
	m.put(it)
	return nil
}

// Close implements Tracker.
func (m *memoryTracker) Close(id string) error {
	it, ok := m.items[id]
	if !ok {
		return fmt.Errorf("item %q not found", id)
	}
	delete(m.items, id)
	m.closed[id] = it
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
	m.put(it)
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
