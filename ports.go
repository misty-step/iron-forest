package main

// Item is one tracker item and its discussion in a host-independent shape. The
// id is a string so a second work source can carry its own identity. The
// GitHub adapter feeds this type through the Tracker port; the controller never
// sees GitHub's native issue shape.
type Item struct {
	ID        string
	Title     string
	Body      string
	UpdatedAt string
	// State is the tracker's own open/closed signal: "open", "closed", or
	// empty when a source does not report it. A branch lane treats a closed
	// Item as stopped work even when a forest branch for it still exists.
	State    string
	Tags     []string
	Comments []comment
}

// Tracker is the work source. A flow lists open items, reads one item, and
// records comments, closure, and tag changes against it.
type Tracker interface {
	// ListOpen returns every open item. A closed item leaves this list, so it
	// needs no reap: the slot frees and nothing can build it.
	ListOpen() ([]Item, error)
	// Get returns one item by its id.
	Get(id string) (Item, error)
	// Comment appends a comment to an item's discussion.
	Comment(id, body string) error
	// Close closes one item.
	// Close returns errTrackerEffectNotApplied only after an exact read proves
	// that a failed close left the item open. Every other failure is uncertain.
	Close(id string) error
	// SetTags adds and removes tags on one item in one call.
	SetTags(id string, add, remove []string) error
}
