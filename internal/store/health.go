package store

import "context"

// Ping verifies that the SQLite handle is still usable for readiness checks.
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}
