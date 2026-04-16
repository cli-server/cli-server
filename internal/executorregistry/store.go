package executorregistry

import "io"

// Store is the database connection. Full implementation in Task 2.
type Store struct {
	io.Closer
}
