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
	Tags      []string
	Comments  []comment
}

// Tracker is the work source. A flow lists open items, reads one item, and
// records comments, closure, and tag changes against it.
type Tracker interface {
	// ListOpen returns every open item.
	ListOpen() ([]Item, error)
	// ListByTag returns every item, open or closed, that carries one tag. The
	// Manager uses it to find ready assignments the open list hides: an item
	// closed by hand still holds forest:ready until the Manager withdraws it.
	ListByTag(tag string) ([]Item, error)
	// Get returns one item by its id.
	Get(id string) (Item, error)
	// Comment appends a comment to an item's discussion.
	Comment(id, body string) error
	// Close closes one item.
	Close(id string) error
	// SetTags adds and removes tags on one item in one call.
	SetTags(id string, add, remove []string) error
}

// Projection is the optional, one-way human surface. It mirrors a branch and
// each decision as a pull request and can merge one.
type Projection interface {
	// FindOpen returns the open projection for a branch, if any.
	FindOpen(branch string) (Item, error)
	// Open publishes a branch as one projection.
	Open(branch, title, body string) (Item, error)
	// Comment appends a comment to an open projection.
	Comment(id, body string) error
	// Merge merges an open projection using the named strategy.
	Merge(id, strategy string) error
}
