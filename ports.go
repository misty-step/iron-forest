package main

// item is one tracker item and its discussion in a host-independent shape. The
// id is a string so a second work source can carry its own identity. It sits
// beside the legacy issue struct; migrating over is #149.
type item struct {
	ID        string
	Title     string
	Body      string
	UpdatedAt string
	Tags      []string
	Comments  []comment
}

// tracker is the work source. A flow lists open items, reads one item, and
// records comments, closure, and tag changes against it.
type tracker interface {
	// ListOpen returns every open item.
	ListOpen() ([]item, error)
	// Get returns one item by its id.
	Get(id string) (item, error)
	// Comment appends a comment to an item's discussion.
	Comment(id, body string) error
	// Close closes one item.
	Close(id string) error
	// SetTags adds and removes tags on one item in one call.
	SetTags(id string, add, remove []string) error
}

// projection is the optional, one-way human surface. It mirrors a branch and
// each decision as a pull request and can merge one.
type projection interface {
	// FindOpen returns the open projection for a branch, if any.
	FindOpen(branch string) (item, error)
	// Open publishes a branch as one projection.
	Open(branch, title, body string) (item, error)
	// Comment appends a comment to an open projection.
	Comment(id, body string) error
	// Merge merges an open projection using the named strategy.
	Merge(id, strategy string) error
}
