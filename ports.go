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
	// ListOpen returns every open item. A closed item leaves this list, so it
	// needs no reap: the slot frees and nothing can build it.
	ListOpen() ([]Item, error)
	// Get returns one item by its id.
	Get(id string) (Item, error)
	// Comment appends a comment to an item's discussion.
	Comment(id, body string) error
	// Close closes one item. It is idempotent: closing an item that is already
	// closed returns nil, so a crash after Close but before a subject's other
	// effects land cannot block a later reconciliation pass from finishing them.
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
